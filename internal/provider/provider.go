// Package provider defines the adapter boundary. The loop speaks this
// interface only; all API-style translation lives behind it.
package provider

import (
	"context"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
)

type Request struct {
	Model    string
	System   string // merged personality file contents
	Messages []envelope.Msg
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type Result struct {
	Text       string
	StopReason string // normalized: end_turn | max_tokens | stop_sequence
	Usage      Usage
}

type Provider interface {
	Chat(ctx context.Context, req Request) (Result, error)
}
