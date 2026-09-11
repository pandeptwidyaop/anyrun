package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pandeptwidyaop/anyrun/internal/emit"
	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
	"github.com/pandeptwidyaop/anyrun/internal/session"
)

type fakeProvider struct {
	gotMessages int
	calls       int
	script      []provider.Result
}

func (f *fakeProvider) Chat(_ context.Context, req provider.Request) (provider.Result, error) {
	f.gotMessages = len(req.Messages)
	f.calls++
	if len(f.script) > 0 {
		res := f.script[0]
		if len(f.script) > 1 {
			f.script = f.script[1:]
		}
		return res, nil
	}
	return provider.Result{Text: "jawaban", StopReason: "end_turn",
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 2}}, nil
}

func userMsg(text string) envelope.Msg {
	return envelope.Msg{Role: "user",
		Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"` + text + `"}`)}}
}

func TestTurnEmitsAssistantAndResultAndPersists(t *testing.T) {
	var out bytes.Buffer
	st := &session.Store{Dir: t.TempDir()}
	fp := &fakeProvider{}

	err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s1", Model: "m", System: "sys",
	}, userMsg("halo"))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	s := out.String()
	if !strings.Contains(s, `"type":"assistant"`) || !strings.Contains(s, "jawaban") {
		t.Errorf("missing assistant event:\n%s", s)
	}
	if !strings.Contains(s, `"type":"result"`) || !strings.Contains(s, `"input_tokens":10`) {
		t.Errorf("missing result event:\n%s", s)
	}

	// Second turn must see the persisted history: user+assistant+user = 3.
	if err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s1", Model: "m", System: "sys",
	}, userMsg("lanjut")); err != nil {
		t.Fatal(err)
	}
	if fp.gotMessages != 3 {
		t.Errorf("provider saw %d messages, want 3 (replayed history)", fp.gotMessages)
	}
}

func toolScript() []provider.Result {
	return []provider.Result{
		{Text: "cek dulu", StopReason: "tool_use",
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "mcp__agent__echo", Args: json.RawMessage(`{"msg":"hai"}`)}},
			Usage:     provider.Usage{InputTokens: 5, OutputTokens: 1}},
		{Text: "hasilnya: echo hai", StopReason: "end_turn",
			Usage: provider.Usage{InputTokens: 7, OutputTokens: 3}},
	}
}

func TestAgenticToolLoop(t *testing.T) {
	var out bytes.Buffer
	st := &session.Store{Dir: t.TempDir()}
	fp := &fakeProvider{script: toolScript()}

	var calledName string
	err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s2", Model: "m", System: "sys",
		Tools: []provider.ToolDef{{Name: "mcp__agent__echo"}},
		CallTool: func(_ context.Context, name string, args json.RawMessage) (string, bool, error) {
			calledName = name
			return "echo: hai", false, nil
		},
	}, userMsg("pakai echo"))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if calledName != "mcp__agent__echo" {
		t.Errorf("tool not called, name=%q", calledName)
	}
	if fp.calls != 2 {
		t.Errorf("provider calls = %d, want 2", fp.calls)
	}

	s := out.String()
	iTool := strings.Index(s, `"type":"tool_use"`)
	iRes := strings.Index(s, `"type":"tool_result"`)
	iFinal := strings.Index(s, "hasilnya")
	if !(iTool >= 0 && iRes > iTool && iFinal > iRes) {
		t.Errorf("event order wrong (tool_use=%d tool_result=%d final=%d):\n%s", iTool, iRes, iFinal, s)
	}
	// usage summed across both provider calls: 5+7 in, 1+3 out
	if !strings.Contains(s, `"input_tokens":12`) || !strings.Contains(s, `"output_tokens":4`) {
		t.Errorf("usage not summed:\n%s", s)
	}

	msgs, _ := st.Load("s2")
	if len(msgs) != 4 { // user, assistant(tool_use), user(tool_result), assistant(final)
		t.Fatalf("persisted %d msgs, want 4", len(msgs))
	}
	if !strings.Contains(string(msgs[1].Content[1]), "tool_use") {
		t.Errorf("assistant tool_use block not persisted: %v", msgs[1])
	}
	if !strings.Contains(string(msgs[2].Content[0]), "tool_result") {
		t.Errorf("tool_result block not persisted: %v", msgs[2])
	}
}

func TestCompactionTriggersAndPersists(t *testing.T) {
	var out bytes.Buffer
	st := &session.Store{Dir: t.TempDir()}

	// Seed 20 messages of history + a fat meta over the threshold.
	var seed []envelope.Msg
	for i := 0; i < 10; i++ {
		seed = append(seed, userMsg("tanya"), envelope.Msg{Role: "assistant",
			Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"jawab"}`)}})
	}
	if err := st.Append("s4", seed...); err != nil {
		t.Fatal(err)
	}
	st.SaveMeta("s4", session.Meta{ContextTokens: 900})

	// Script: first Chat = summarization, second = the actual reply.
	fp := &fakeProvider{script: []provider.Result{
		{Text: "RINGKASAN penting", StopReason: "end_turn", Usage: provider.Usage{InputTokens: 50, OutputTokens: 5}},
		{Text: "balasan", StopReason: "end_turn", Usage: provider.Usage{InputTokens: 30, OutputTokens: 4}},
	}}

	err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s4", Model: "m",
		ContextWindow: 1000, CompactAt: 0.8,
	}, userMsg("halo lagi"))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	s := out.String()
	if !strings.Contains(s, `"type":"compact_boundary"`) || !strings.Contains(s, "RINGKASAN penting") {
		t.Errorf("missing compact_boundary event:\n%s", s)
	}

	msgs, _ := st.Load("s4")
	if len(msgs) >= 22 {
		t.Errorf("history not compacted: %d msgs", len(msgs))
	}
	if !strings.Contains(string(msgs[0].Content[0]), "Ringkasan percakapan") {
		t.Errorf("summary not at head: %s", msgs[0].Content[0])
	}

	meta, _ := st.LoadMeta("s4")
	if meta.CompactCount != 1 {
		t.Errorf("compact count = %d, want 1", meta.CompactCount)
	}
	if meta.ContextTokens != 34 { // last call: 30 in + 4 out
		t.Errorf("context tokens = %d, want 34", meta.ContextTokens)
	}
	// usage totals include the summarization call: 50+30 / 5+4
	if !strings.Contains(s, `"input_tokens":80`) || !strings.Contains(s, `"output_tokens":9`) {
		t.Errorf("usage missing summarization cost:\n%s", s)
	}
}

func TestNoCompactionBelowThreshold(t *testing.T) {
	var out bytes.Buffer
	st := &session.Store{Dir: t.TempDir()}
	st.Append("s5", userMsg("a"))
	st.SaveMeta("s5", session.Meta{ContextTokens: 100})
	fp := &fakeProvider{}
	if err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s5", Model: "m", ContextWindow: 1000, CompactAt: 0.8,
	}, userMsg("b")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "compact_boundary") {
		t.Error("compaction fired below threshold")
	}
	if fp.calls != 1 {
		t.Errorf("provider calls = %d, want 1", fp.calls)
	}
}

func TestMaxTurnsStopsLoop(t *testing.T) {
	var out bytes.Buffer
	st := &session.Store{Dir: t.TempDir()}
	// Script forever returns tool calls — only MaxTurns can stop it.
	fp := &fakeProvider{script: []provider.Result{
		{StopReason: "tool_use",
			ToolCalls: []provider.ToolCall{{ID: "c", Name: "mcp__agent__echo", Args: json.RawMessage(`{}`)}}},
	}}
	err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s3", Model: "m", MaxTurns: 2,
		CallTool: func(_ context.Context, _ string, _ json.RawMessage) (string, bool, error) {
			return "ok", false, nil
		},
	}, userMsg("loop"))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if fp.calls != 2 {
		t.Errorf("provider calls = %d, want 2 (capped)", fp.calls)
	}
	if !strings.Contains(out.String(), `"stop_reason":"max_turns"`) {
		t.Errorf("missing max_turns stop reason:\n%s", out.String())
	}
}

// killProvider serves tool calls until killAfter, then cancels the context —
// standing in for claude-agent's SIGKILL on timeout.
type killProvider struct {
	cancel    context.CancelFunc
	killAfter int
	calls     int
}

func (k *killProvider) Chat(_ context.Context, _ provider.Request) (provider.Result, error) {
	k.calls++
	if k.calls > k.killAfter {
		k.cancel()
		return provider.Result{}, context.Canceled
	}
	return provider.Result{
		StopReason: "tool_use",
		ToolCalls:  []provider.ToolCall{{ID: "c", Name: "bash", Args: json.RawMessage(`{}`)}},
		Usage:      provider.Usage{InputTokens: 10, OutputTokens: 2},
	}, nil
}

// The regression test for the reported bug: a run killed mid-flight used to
// discard the ENTIRE turn, including tools that had already executed.
func TestKilledRunKeepsCompletedSteps(t *testing.T) {
	var out bytes.Buffer
	st := &session.Store{Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kp := &killProvider{cancel: cancel, killAfter: 2}
	err := Turn(ctx, Deps{
		Provider: kp, Store: st, Emit: emit.New(&out),
		SessionID: "s", Model: "m",
		CallTool: func(_ context.Context, _ string, _ json.RawMessage) (string, bool, error) {
			return "ok", false, nil
		},
	}, userMsg("kerjakan tugas panjang"))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	msgs, err := st.Load("s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Two completed tool rounds => user + (assistant, tool_result) * 2
	// The third assistant call never returned, so nothing more is written.
	if len(msgs) != 5 {
		t.Fatalf("persisted %d messages, want 5 (steps already done):\n%+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].Role = %q, want user", msgs[0].Role)
	}
	if msgs[len(msgs)-1].Role != "user" {
		t.Errorf("last message = %q, want the trailing tool_result", msgs[len(msgs)-1].Role)
	}
}

// A run killed before the model ever answered must leave nothing: the retried
// job resends the same user message, and a lone stored userMsg would duplicate.
func TestKilledBeforeFirstAnswerPersistsNothing(t *testing.T) {
	var out bytes.Buffer
	st := &session.Store{Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kp := &killProvider{cancel: cancel, killAfter: 0}
	err := Turn(ctx, Deps{
		Provider: kp, Store: st, Emit: emit.New(&out),
		SessionID: "s2", Model: "m",
		CallTool: func(_ context.Context, _ string, _ json.RawMessage) (string, bool, error) {
			return "ok", false, nil
		},
	}, userMsg("halo"))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	msgs, err := st.Load("s2")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("persisted %d messages, want 0 (no model answer yet): %+v", len(msgs), msgs)
	}
}

// The final answer of a normal turn must still land on disk.
func TestCompletedTurnPersistsFinalText(t *testing.T) {
	var out bytes.Buffer
	st := &session.Store{Dir: t.TempDir()}
	if err := Turn(context.Background(), Deps{
		Provider: &fakeProvider{}, Store: st, Emit: emit.New(&out),
		SessionID: "s3", Model: "m",
	}, userMsg("halo")); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	msgs, err := st.Load("s3")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("history = %d, want 2 (user + assistant)", len(msgs))
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}
}

// --- dangling tool_use repair ---------------------------------------------

func assistantWithToolUse(ids ...string) envelope.Msg {
	content := []json.RawMessage{rawBlock(`{"type":"text","text":"sebentar"}`)}
	for _, id := range ids {
		content = append(content, rawBlock(
			`{"type":"tool_use","id":"`+id+`","name":"bash","input":{}}`))
	}
	return envelope.Msg{Role: "assistant", Content: content}
}

func rawBlock(s string) json.RawMessage { return json.RawMessage(s) }

// A kill between "assistant asked for a tool" and "the tool reported back" is
// the one hole incremental persistence opens; the history must be closed before
// it reaches the provider.
func TestRepairClosesDanglingToolUse(t *testing.T) {
	history := []envelope.Msg{
		userMsg("kerjakan"),
		assistantWithToolUse("toolu_1"),
	}
	got, ok := repairDanglingToolUse(history)
	if !ok {
		t.Fatal("want repair for a history ending in tool_use")
	}
	if len(got) != 3 {
		t.Fatalf("history = %d, want 3", len(got))
	}
	last := got[2]
	if last.Role != "user" {
		t.Errorf("role = %q, want user", last.Role)
	}
	var block struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
	}
	if err := json.Unmarshal(last.Content[0], &block); err != nil {
		t.Fatal(err)
	}
	if block.Type != "tool_result" || block.ToolUseID != "toolu_1" {
		t.Errorf("block = %+v, want tool_result for toolu_1", block)
	}
}

// Parallel tool calls interrupted together each need their own result, or the
// provider still sees unmatched ids.
func TestRepairHandlesParallelToolCalls(t *testing.T) {
	got, ok := repairDanglingToolUse([]envelope.Msg{
		userMsg("kerjakan"),
		assistantWithToolUse("a", "b", "c"),
	})
	if !ok {
		t.Fatal("want repair")
	}
	if len(got) != 3 || len(got[2].Content) != 3 {
		t.Fatalf("want 3 synthetic results, got %d", len(got[2].Content))
	}
	for i, id := range []string{"a", "b", "c"} {
		var block struct {
			ToolUseID string `json:"tool_use_id"`
		}
		_ = json.Unmarshal(got[2].Content[i], &block)
		if block.ToolUseID != id {
			t.Errorf("result %d id = %q, want %q", i, block.ToolUseID, id)
		}
	}
}

// Everything that is NOT a dangling tool_use must pass through untouched.
func TestRepairLeavesHealthyHistoriesAlone(t *testing.T) {
	cases := map[string][]envelope.Msg{
		"empty": {},
		"assistant text": {userMsg("hai"), envelope.Msg{Role: "assistant",
			Content: []json.RawMessage{rawBlock(`{"type":"text","text":"halo"}`)}}},
		"tool_result last": {userMsg("hai"), assistantWithToolUse("t"),
			envelope.Msg{Role: "user", Content: []json.RawMessage{
				rawBlock(`{"type":"tool_result","tool_use_id":"t","content":"ok"}`)}}},
		"user last": {userMsg("hai")},
	}
	for name, history := range cases {
		got, ok := repairDanglingToolUse(history)
		if ok {
			t.Errorf("%s: repaired when it should not have", name)
		}
		if len(got) != len(history) {
			t.Errorf("%s: length changed %d -> %d", name, len(history), len(got))
		}
	}
}

// End to end: a session left mid-tool by a previous kill must still resume, and
// the provider must receive a history whose tool calls are all answered.
func TestInterruptedSessionResumesWithClosedHistory(t *testing.T) {
	st := &session.Store{Dir: t.TempDir()}
	// Exactly what the previous run wrote before it was killed.
	if err := st.Append("s9", userMsg("kerjakan"),
		assistantWithToolUse("toolu_9")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fp := &fakeProvider{}
	if err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s9", Model: "m",
	}, userMsg("lanjut")); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	// user, assistant(tool_use), synthetic tool_result, resume note, user("lanjut").
	// Without the repair the provider would see 3 and reject the history.
	if fp.gotMessages != 5 {
		t.Errorf("provider saw %d messages, want 5 (synthetic result + resume note)", fp.gotMessages)
	}
	// The synthetic result MUST be persisted. Leaving the file ending in a bare
	// assistant(tool_use) means the next flush writes the new user message right
	// after it, and the following turn carries a tool_call with no tool message
	// (OpenAI 400). Persisting closes the pair once and for all.
	msgs, err := st.Load("s9")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("persisted %d messages, want 5 (user, assistant(tool_use), synthetic tool_result, user, assistant)", len(msgs))
	}
	if msgs[1].Role != "assistant" || msgs[2].Role != "user" {
		t.Fatalf("tool_use/tool_result pairing wrong: %q then %q", msgs[1].Role, msgs[2].Role)
	}
	var block struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
	}
	if err := json.Unmarshal(msgs[2].Content[0], &block); err != nil {
		t.Fatal(err)
	}
	if block.Type != "tool_result" || block.ToolUseID != "toolu_9" {
		t.Errorf("msgs[2] = %+v, want synthetic tool_result for toolu_9", block)
	}

	// A second resume over the now-closed file must load cleanly and send a valid
	// history to the provider (no dangling tool_call mid-history).
	fp2 := &fakeProvider{}
	if err := Turn(context.Background(), Deps{
		Provider: fp2, Store: st, Emit: emit.New(&out),
		SessionID: "s9", Model: "m",
	}, userMsg("lagi")); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	// history(5) + user("lagi") = 6. No resume note this time — the first resume
	// completed cleanly with a final answer, so the run is not "cut short".
	if fp2.gotMessages != 6 {
		t.Errorf("second resume: provider saw %d, want 6 (no dangling tool_call)", fp2.gotMessages)
	}
}

// --- resume note -----------------------------------------------------------

// A tool result the model never got to react to means the run died mid-flight —
// the shape the worker sees when it retries a timed-out job.
func TestCutShortHistoryEndingInToolResult(t *testing.T) {
	history := []envelope.Msg{
		userMsg("kerjakan"),
		assistantWithToolUse("t"),
		{Role: "user", Content: []json.RawMessage{
			rawBlock(`{"type":"tool_result","tool_use_id":"t","content":"ok"}`)}},
	}
	if !historyWasCutShort(history) {
		t.Error("history ending in tool_result is unfinished work")
	}
}

func TestFinishedHistoryIsNotCutShort(t *testing.T) {
	cases := map[string][]envelope.Msg{
		"empty": {},
		"assistant text last": {userMsg("hai"), {Role: "assistant",
			Content: []json.RawMessage{rawBlock(`{"type":"text","text":"halo"}`)}}},
		"user last": {userMsg("hai")},
	}
	for name, history := range cases {
		if historyWasCutShort(history) {
			t.Errorf("%s: must not be treated as cut short", name)
		}
	}
}

// The note is the difference between the model reading the history and acting
// on it. Without it a redelivered request restarts from scratch.
func TestResumeNoteInjectedForCutShortHistory(t *testing.T) {
	st := &session.Store{Dir: t.TempDir()}
	if err := st.Append("s1", userMsg("kerjakan"), assistantWithToolUse("t"),
		envelope.Msg{Role: "user", Content: []json.RawMessage{
			rawBlock(`{"type":"tool_result","tool_use_id":"t","content":"ok"}`)}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fp := &fakeProvider{}
	if err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out), SessionID: "s1", Model: "m",
	}, userMsg("kerjakan")); err != nil {
		t.Fatal(err)
	}
	// 3 stored + note + redelivered request
	if fp.gotMessages != 5 {
		t.Fatalf("provider saw %d messages, want 5 (note injected)", fp.gotMessages)
	}

	// The note must not be persisted: it is guidance, not history.
	msgs, err := st.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		for _, raw := range m.Content {
			if strings.Contains(string(raw), "Run sebelumnya terputus") {
				t.Errorf("resume note leaked into the session file: %s", raw)
			}
		}
	}
	// 3 stored + the redelivered request + the answer the fake provider gave
	if len(msgs) != 5 {
		t.Errorf("persisted %d messages, want 5", len(msgs))
	}
}

// A clean history must not be annotated — the note would be a lie.
func TestNoResumeNoteForFinishedHistory(t *testing.T) {
	st := &session.Store{Dir: t.TempDir()}
	if err := st.Append("s2", userMsg("hai"), envelope.Msg{Role: "assistant",
		Content: []json.RawMessage{rawBlock(`{"type":"text","text":"halo"}`)}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	fp := &fakeProvider{}
	if err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out), SessionID: "s2", Model: "m",
	}, userMsg("lanjut")); err != nil {
		t.Fatal(err)
	}
	if fp.gotMessages != 3 {
		t.Errorf("provider saw %d messages, want 3 (no note)", fp.gotMessages)
	}
}
