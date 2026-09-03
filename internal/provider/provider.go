// Package provider defines the adapter boundary. The loop speaks this
// interface only; all API-style translation lives behind it.
package provider

import (
	"context"
	"encoding/json"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
)

type ToolDef struct {
	Name        string // mcp__<server>__<tool>
	Description string
	Schema      json.RawMessage
}

type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage // JSON object
}

type Request struct {
	Model    string
	System   string // merged personality file contents
	Messages []envelope.Msg
	Tools    []ToolDef
}

type Usage struct {
	InputTokens  int // uncached input tokens (claude-CLI semantics: excludes CacheRead)
	CacheRead    int // input tokens served from the provider's prompt cache
	OutputTokens int
}

type Result struct {
	Text       string
	ToolCalls  []ToolCall
	StopReason string // normalized: end_turn | max_tokens | stop_sequence | tool_use
	Usage      Usage
}

type Provider interface {
	Chat(ctx context.Context, req Request) (Result, error)
}
