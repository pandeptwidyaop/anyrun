// Package emit writes the stream-json subset claude-agent's parser consumes.
// Contract (claude-agent internal/claude/runner.go parseStreamLine):
//
//	assistant -> message.content[] blocks: text | thinking | tool_use
//	user      -> message.content[] blocks: tool_result
//	result    -> result, stop_reason, duration_ms, total_cost_usd,
//	             is_error, errors[], usage{...}, modelUsage{m:{contextWindow}}
//
// Nothing else may be written to stdout — stray prints corrupt the stream.
package emit

import (
	"encoding/json"
	"io"
	"sync"
)

type Writer struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func New(w io.Writer) *Writer { return &Writer{enc: json.NewEncoder(w)} }

func (w *Writer) write(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(v)
}

// Init mirrors the CLI's first line. The parser ignores it, but it is cheap
// insurance for anything else that tails the stream.
func (w *Writer) Init(sessionID, model string) error {
	return w.write(map[string]any{
		"type": "system", "subtype": "init",
		"session_id": sessionID, "model": model,
	})
}

func (w *Writer) AssistantText(text string) error {
	return w.write(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
}

type ResultInfo struct {
	Text          string
	StopReason    string
	DurationMS    int64
	InputTokens   int
	OutputTokens  int
	Model         string
	ContextWindow int // 0 = omit modelUsage
}

func (w *Writer) Result(r ResultInfo) error {
	m := map[string]any{
		"type": "result", "subtype": "success",
		"result":      r.Text,
		"stop_reason": r.StopReason,
		"duration_ms": r.DurationMS,
		// anyrun does not price tokens; claude-agent's models collection owns
		// pricing and computes cost from the usage fields below.
		"total_cost_usd": 0.0,
		"is_error":       false,
		"usage": map[string]any{
			"input_tokens":                r.InputTokens,
			"output_tokens":               r.OutputTokens,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 0,
		},
	}
	if r.ContextWindow > 0 {
		m["modelUsage"] = map[string]any{r.Model: map[string]any{"contextWindow": r.ContextWindow}}
	}
	return w.write(m)
}

func (w *Writer) ResultError(msg string, durationMS int64) error {
	return w.write(map[string]any{
		"type": "result", "subtype": "error",
		"result": msg, "errors": []string{msg},
		"duration_ms": durationMS, "total_cost_usd": 0.0,
		"is_error": true,
	})
}
