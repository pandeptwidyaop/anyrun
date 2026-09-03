// Package openai adapts the provider interface to OpenAI-style
// /chat/completions APIs (OpenRouter, vLLM, DeepSeek, ...).
//
// Milestone 1 scope: text blocks only, no tools, non-streaming. The
// observable behavior toward claude-agent is identical either way, because
// its parser consumes complete messages, not deltas.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
)

type Client struct {
	BaseURL string // e.g. https://openrouter.ai/api/v1
	APIKey  string
	HTTP    *http.Client
}

type oaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// flattenText joins the text blocks of an Anthropic-shaped message.
// Non-text blocks (image/document) are out of scope in milestone 1 and are
// replaced by a placeholder so the model at least knows something was there.
func flattenText(m envelope.Msg) string {
	var parts []string
	for _, raw := range m.Content {
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		default:
			parts = append(parts, fmt.Sprintf("[unsupported %s block omitted]", b.Type))
		}
	}
	return strings.Join(parts, "\n")
}

func normalizeStop(reason string) string {
	switch reason {
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		return reason
	}
}

func (c *Client) Chat(ctx context.Context, req provider.Request) (provider.Result, error) {
	msgs := make([]oaMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, oaMessage{Role: m.Role, Content: flattenText(m)})
	}

	body, err := json.Marshal(map[string]any{
		"model":    req.Model,
		"messages": msgs,
	})
	if err != nil {
		return provider.Result{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return provider.Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return provider.Result{}, fmt.Errorf("openai: request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return provider.Result{}, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out struct {
		Choices []struct {
			Message      oaMessage `json:"message"`
			FinishReason string    `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return provider.Result{}, fmt.Errorf("openai: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return provider.Result{}, fmt.Errorf("openai: empty choices: %s", string(raw))
	}
	return provider.Result{
		Text:       out.Choices[0].Message.Content,
		StopReason: normalizeStop(out.Choices[0].FinishReason),
		Usage: provider.Usage{
			InputTokens:  out.Usage.PromptTokens,
			OutputTokens: out.Usage.CompletionTokens,
		},
	}, nil
}
