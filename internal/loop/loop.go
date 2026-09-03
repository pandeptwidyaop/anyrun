// Package loop runs one conversational turn: load history, call the
// provider, emit contract events, persist. (The agentic multi-turn tool
// loop arrives with the MCP milestone; the seam is already here.)
package loop

import (
	"context"
	"encoding/json"
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
}

func Turn(ctx context.Context, d Deps, userMsg envelope.Msg) error {
	start := time.Now()

	history, err := d.Store.Load(d.SessionID)
	if err != nil {
		return err
	}
	messages := append(history, userMsg)

	res, err := d.Provider.Chat(ctx, provider.Request{
		Model: d.Model, System: d.System, Messages: messages,
	})
	if err != nil {
		return err
	}

	if err := d.Emit.AssistantText(res.Text); err != nil {
		return err
	}

	assistantMsg := envelope.Msg{
		Role:    "assistant",
		Content: []json.RawMessage{mustBlock(res.Text)},
	}
	// Persist AFTER the provider succeeded: a failed call must not leave a
	// user message in history that the model never saw.
	if err := d.Store.Append(d.SessionID, userMsg, assistantMsg); err != nil {
		return err
	}

	return d.Emit.Result(emit.ResultInfo{
		Text:          res.Text,
		StopReason:    res.StopReason,
		DurationMS:    time.Since(start).Milliseconds(),
		InputTokens:   res.Usage.InputTokens,
		OutputTokens:  res.Usage.OutputTokens,
		Model:         d.Model,
		ContextWindow: d.ContextWindow,
	})
}

func mustBlock(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": "text", "text": text})
	return b
}
