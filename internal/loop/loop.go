// Package loop runs the agentic conversation: load history, call the
// provider, execute tool calls via the MCP bridge, repeat until the model
// answers with plain text (or --max-turns trips), then persist and report.
package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
	MaxTurns      int // 0 = unlimited
}

func Turn(ctx context.Context, d Deps, userMsg envelope.Msg) error {
	start := time.Now()

	history, err := d.Store.Load(d.SessionID)
	if err != nil {
		return err
	}
	msgs := append(history, userMsg)
	newMsgs := []envelope.Msg{userMsg}

	var total provider.Usage
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
			return err
		}
		total.InputTokens += res.Usage.InputTokens
		total.OutputTokens += res.Usage.OutputTokens

		if err := d.Emit.AssistantTurn(res.Text, res.ToolCalls); err != nil {
			return err
		}
		aMsg := assistantMsg(res.Text, res.ToolCalls)
		msgs = append(msgs, aMsg)
		newMsgs = append(newMsgs, aMsg)

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
		newMsgs = append(newMsgs, rMsg)
	}

	// Persist AFTER the run succeeded: a failed provider call must not
	// leave messages in history the model never saw. (Executed tool side
	// effects on a mid-run failure are accepted — same as the real CLI.)
	if err := d.Store.Append(d.SessionID, newMsgs...); err != nil {
		return err
	}

	return d.Emit.Result(emit.ResultInfo{
		Text:          finalText,
		StopReason:    stop,
		DurationMS:    time.Since(start).Milliseconds(),
		InputTokens:   total.InputTokens,
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
		b, _ := json.Marshal(map[string]any{
			"type": "tool_use", "id": c.ID, "name": c.Name, "input": input,
		})
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
