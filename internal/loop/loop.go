// Package loop runs the agentic conversation: load history, call the
// provider, execute tool calls via the MCP bridge, repeat until the model
// answers with plain text (or --max-turns trips), then persist and report.
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/pandeptwidyaop/anyrun/internal/compact"
	"github.com/pandeptwidyaop/anyrun/internal/emit"
	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
	"github.com/pandeptwidyaop/anyrun/internal/session"
)

type Deps struct {
	Provider      provider.Provider
	Store         *session.Store
	Emit          *emit.Writer
	SessionID     string
	Model         string
	System        string
	ContextWindow int // from ANYRUN_CONTEXT_WINDOW; 0 = unknown
	Tools         []provider.ToolDef
	CallTool      func(ctx context.Context, name string, args json.RawMessage) (string, bool, error)
	MaxTurns      int     // 0 = unlimited
	CompactAt     float64 // fraction of ContextWindow that triggers compaction; 0 = disabled
}

// interruptedNote is what a tool call reports when the run that requested it
// was killed before it could report back. It is deliberately explicit: the
// alternative is a model that assumes the tool succeeded.
const interruptedNote = "run sebelumnya terputus sebelum tool ini selesai — hasilnya tidak diketahui"

// repairDanglingToolUse closes a history that was cut off between an assistant
// tool_use and its results. This is the one shape incremental persistence can
// leave behind: the assistant message is written before the tools execute (so a
// resume knows what was about to happen), which by definition means a kill
// during tool execution leaves the pair half-written.
//
// Left alone, that history is not merely incomplete — OpenAI-compatible
// providers reject it outright, every tool_call needing a matching tool
// message, so the session would become unusable rather than resumable.
//
// The inserted result is not persisted: it is a view for this run only, and is
// regenerated if the same interruption happens again.
func repairDanglingToolUse(history []envelope.Msg) ([]envelope.Msg, bool) {
	if len(history) == 0 {
		return history, false
	}
	last := history[len(history)-1]
	if last.Role != "assistant" {
		return history, false
	}

	var ids []string
	for _, raw := range last.Content {
		var block struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			continue
		}
		if block.Type == "tool_use" && block.ID != "" {
			ids = append(ids, block.ID)
		}
	}
	if len(ids) == 0 {
		return history, false
	}

	content := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		block, err := json.Marshal(map[string]string{
			"type": "tool_result", "tool_use_id": id, "content": interruptedNote,
		})
		if err != nil {
			return history, false
		}
		content = append(content, block)
	}
	return append(history, envelope.Msg{Role: "user", Content: content}), true
}

// resumeNote is injected when the history shows a run that never finished.
// Without it the model has the facts but no reason to trust them: a redelivered
// request (the worker retries a timed-out job with the identical message) reads
// as a fresh instruction, so the model starts over and repeats work that
// already happened. With side effects attached, repeating is destructive.
const resumeNote = "[catatan] Run sebelumnya terputus sebelum selesai. Riwayat di atas " +
	"memuat langkah yang SUDAH dijalankan beserta hasilnya. Permintaan di bawah " +
	"mungkin pengiriman ulang dari permintaan yang sama. Jangan mengulangi langkah " +
	"yang sudah selesai — lanjutkan dari yang belum, dan sebutkan mana yang sudah beres."

// historyWasCutShort reports whether the history ends mid-run: either an
// assistant tool_use left unanswered, or a tool result the model never got to
// react to. Both mean the last thing on record is work in progress, not an
// answer.
func historyWasCutShort(history []envelope.Msg) bool {
	if len(history) == 0 {
		return false
	}
	last := history[len(history)-1]
	if last.Role != "user" {
		return false
	}
	if len(last.Content) == 0 {
		return false
	}
	var block struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(last.Content[0], &block) != nil {
		return false
	}
	return block.Type == "tool_result"
}

func Turn(ctx context.Context, d Deps, userMsg envelope.Msg) error {
	start := time.Now()

	history, err := d.Store.Load(d.SessionID)
	if err != nil {
		return err
	}
	repaired := false
	if closed, ok := repairDanglingToolUse(history); ok {
		fmt.Fprintf(os.Stderr, "anyrun: session %s: repaired dangling tool_use from an interrupted run\n", d.SessionID)
		history = closed
		repaired = true
	}

	var total provider.Usage
	meta, _ := d.Store.LoadMeta(d.SessionID)

	// Compaction check: the previous turn's provider-reported context size
	// against the configured threshold. Failure degrades to "carry on with
	// the big context" — never lose the conversation over housekeeping.
	if d.ContextWindow > 0 && d.CompactAt > 0 &&
		meta.ContextTokens > int(float64(d.ContextWindow)*d.CompactAt) {
		if cut := compact.CutIndex(history, 0.25); cut > 0 {
			pre := meta.ContextTokens
			_ = d.Emit.CompactStart(pre, d.ContextWindow)
			summary, u, err := compact.Summarize(ctx, d.Provider, d.Model, history[:cut])
			total.InputTokens += u.InputTokens
			total.CacheRead += u.CacheRead
			total.OutputTokens += u.OutputTokens
			if err != nil {
				fmt.Fprintf(os.Stderr, "anyrun: compaction failed, continuing uncompacted: %v\n", err)
			} else if err := compact.Rewrite(d.Store, d.SessionID, summary, history[cut:]); err != nil {
				fmt.Fprintf(os.Stderr, "anyrun: compaction rewrite failed: %v\n", err)
			} else {
				if history, err = d.Store.Load(d.SessionID); err != nil {
					return err
				}
				_ = d.Emit.CompactBoundary(summary, pre)
				meta.CompactCount++
			}
		}
	}

	// The note is a view for this run only — never persisted, so it cannot
	// accumulate, and the file keeps the real record.
	msgs := history
	if repaired || historyWasCutShort(history) {
		note, _ := json.Marshal(map[string]string{"type": "text", "text": resumeNote})
		msgs = append(msgs, envelope.Msg{Role: "user", Content: []json.RawMessage{note}})
	}
	msgs = append(msgs, userMsg)

	// Session state is persisted per step, not per turn. A run can be killed at
	// any moment — claude-agent SIGKILLs the whole process group on timeout —
	// and SIGKILL cannot be caught. Waiting until the loop finishes throws away
	// every tool call that already ran and had real side effects, which is the
	// "the agent forgot where it stopped" failure: the work happened, the record
	// of it did not.
	//
	// userMsg rides along with the first assistant message instead of being
	// written up front. If the run dies before the model answers, nothing is
	// persisted and a retry re-sends the user message cleanly rather than
	// duplicating it.
	pending := []envelope.Msg{userMsg}
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := d.Store.Append(d.SessionID, pending...); err != nil {
			return err
		}
		pending = pending[:0]
		return nil
	}

	finalText, stop := "", "end_turn"

	for turn := 0; ; turn++ {
		if d.MaxTurns > 0 && turn >= d.MaxTurns {
			stop = "max_turns"
			break
		}

		res, err := d.Provider.Chat(ctx, provider.Request{
			Model: d.Model, System: d.System, Messages: msgs, Tools: d.Tools,
		})
		if err != nil {
			// No flush here on purpose: at this point `pending` can only hold
			// userMsg, and writing it alone would duplicate the message when
			// the job is retried. Every completed step was already flushed at
			// its own boundary, so nothing is lost by returning directly.
			return err
		}
		total.InputTokens += res.Usage.InputTokens
		total.CacheRead += res.Usage.CacheRead
		total.OutputTokens += res.Usage.OutputTokens
		// Context size for the NEXT turn's compaction decision: everything
		// the provider just saw (cached or not) plus what it produced.
		meta.ContextTokens = res.Usage.InputTokens + res.Usage.CacheRead + res.Usage.OutputTokens

		if err := d.Emit.AssistantTurn(res.Text, res.ToolCalls); err != nil {
			return err
		}
		aMsg := assistantMsg(res.Text, res.ToolCalls)
		msgs = append(msgs, aMsg)
		// Written before the tools run: "the model decided to call these" is
		// the record that tells a resumed run where it stopped.
		pending = append(pending, aMsg)
		if err := flush(); err != nil {
			return err
		}

		if len(res.ToolCalls) == 0 {
			finalText, stop = res.Text, res.StopReason
			break
		}
		if d.CallTool == nil {
			return fmt.Errorf("loop: model requested tools but no tool executor is wired")
		}

		results := make([]emit.ToolResult, 0, len(res.ToolCalls))
		for _, call := range res.ToolCalls {
			content, isErr, err := d.CallTool(ctx, call.Name, call.Args)
			if err != nil {
				content, isErr = fmt.Sprintf("tool execution failed: %v", err), true
			}
			if isErr {
				content = "ERROR: " + content
			}
			results = append(results, emit.ToolResult{ID: call.ID, Content: content})
		}
		if err := d.Emit.ToolResults(results); err != nil {
			return err
		}
		rMsg := toolResultMsg(results)
		msgs = append(msgs, rMsg)
		// Results land in the same flush as their tool_use pairing requires;
		// providers reject a tool_call whose tool message never arrives.
		pending = append(pending, rMsg)
		if err := flush(); err != nil {
			return err
		}
	}

	// Every step already flushed as it completed; this only drains anything a
	// future edit might leave buffered, so it can never silently go missing.
	if err := flush(); err != nil {
		return err
	}
	if err := d.Store.SaveMeta(d.SessionID, meta); err != nil {
		fmt.Fprintf(os.Stderr, "anyrun: save meta: %v\n", err)
	}

	return d.Emit.Result(emit.ResultInfo{
		Text:          finalText,
		StopReason:    stop,
		DurationMS:    time.Since(start).Milliseconds(),
		InputTokens:   total.InputTokens,
		CacheRead:     total.CacheRead,
		OutputTokens:  total.OutputTokens,
		Model:         d.Model,
		ContextWindow: d.ContextWindow,
	})
}

func assistantMsg(text string, calls []provider.ToolCall) envelope.Msg {
	var content []json.RawMessage
	if text != "" {
		b, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		content = append(content, b)
	}
	for _, c := range calls {
		input := json.RawMessage(c.Args)
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		block := map[string]any{
			"type": "tool_use", "id": c.ID, "name": c.Name, "input": input,
		}
		// Provider-specific extras (e.g. Gemini 3 thought signatures) ride
		// along so history replay can echo the original object verbatim.
		if len(c.Raw) > 0 {
			block["raw_tool_call"] = json.RawMessage(c.Raw)
		}
		b, _ := json.Marshal(block)
		content = append(content, b)
	}
	return envelope.Msg{Role: "assistant", Content: content}
}

func toolResultMsg(results []emit.ToolResult) envelope.Msg {
	var content []json.RawMessage
	for _, r := range results {
		b, _ := json.Marshal(map[string]any{
			"type": "tool_result", "tool_use_id": r.ID, "content": r.Content,
		})
		content = append(content, b)
	}
	return envelope.Msg{Role: "user", Content: content}
}
