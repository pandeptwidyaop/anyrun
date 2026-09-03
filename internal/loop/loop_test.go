package loop

import (
	"bytes"
	"context"
	"encoding/json"
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
