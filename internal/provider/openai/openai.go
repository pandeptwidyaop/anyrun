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
	"os"
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

// block is the superset of Anthropic block fields anyrun cares about.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   string          `json:"content"`
	// RawToolCall is the provider's original tool_call object, stored by
	// the loop so replay preserves provider-specific fields.
	RawToolCall json.RawMessage `json:"raw_tool_call"`
}

func decodeBlocks(m envelope.Msg) []block {
	out := make([]block, 0, len(m.Content))
	for _, raw := range m.Content {
		var b block
		if json.Unmarshal(raw, &b) == nil {
			out = append(out, b)
		}
	}
	return out
}

// toOpenAI translates Anthropic-shaped history into chat-completions
// messages. tool_use lands on the assistant message as tool_calls;
// each tool_result becomes its own role=tool message (OpenAI's required
// shape). Non-text user blocks (image/document) become placeholders —
// media translation is a later milestone.
func toOpenAI(system string, msgs []envelope.Msg) []map[string]any {
	out := make([]map[string]any, 0, len(msgs)+1)
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, m := range msgs {
		blocks := decodeBlocks(m)

		if m.Role == "user" {
			var toolResults []block
			var textParts []string
			for _, b := range blocks {
				switch b.Type {
				case "tool_result":
					toolResults = append(toolResults, b)
				case "text":
					textParts = append(textParts, b.Text)
				default:
					textParts = append(textParts, fmt.Sprintf("[unsupported %s block omitted]", b.Type))
				}
			}
			for _, tr := range toolResults {
				out = append(out, map[string]any{
					"role": "tool", "tool_call_id": tr.ToolUseID, "content": tr.Content,
				})
			}
			if len(textParts) > 0 {
				out = append(out, map[string]any{"role": "user", "content": strings.Join(textParts, "\n")})
			}
			continue
		}

		// assistant
		var text []string
		var toolCalls []any
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text = append(text, b.Text)
			case "tool_use":
				// Prefer the provider's original object — extra fields like
				// Gemini 3's thought_signature MUST round-trip or the API 400s.
				if len(b.RawToolCall) > 0 {
					toolCalls = append(toolCalls, json.RawMessage(b.RawToolCall))
					continue
				}
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				toolCalls = append(toolCalls, map[string]any{
					"id": b.ID, "type": "function",
					"function": map[string]any{"name": b.Name, "arguments": args},
				})
			}
		}
		am := map[string]any{"role": "assistant", "content": strings.Join(text, "\n")}
		if len(toolCalls) > 0 {
			am["tool_calls"] = toolCalls
		}
		out = append(out, am)
	}
	return out
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
	payload := map[string]any{
		"model":    req.Model,
		"messages": toOpenAI(req.System, req.Messages),
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, d := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        d.Name,
					"description": d.Description,
					"parameters":  json.RawMessage(d.Schema),
				},
			})
		}
		payload["tools"] = tools
	}
	body, err := json.Marshal(payload)
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
			Message struct {
				Content string `json:"content"`
				// Raw first: providers attach extra fields (Gemini 3's
				// extra_content.thought_signature) that must survive replay.
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			// DeepSeek-style cache reporting.
			PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
			// OpenAI-style cache reporting.
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return provider.Result{}, fmt.Errorf("openai: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return provider.Result{}, fmt.Errorf("openai: empty choices: %s", string(raw))
	}
	choice := out.Choices[0]

	var calls []provider.ToolCall
	for _, raw := range choice.Message.ToolCalls {
		var tc struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &tc); err != nil {
			fmt.Fprintf(os.Stderr, "anyrun: undecodable tool call skipped: %v\n", err)
			continue
		}
		args := json.RawMessage(tc.Function.Arguments)
		if !json.Valid(args) || len(args) == 0 {
			fmt.Fprintf(os.Stderr, "anyrun: tool call %s: invalid arguments %q, using {}\n", tc.Function.Name, tc.Function.Arguments)
			args = json.RawMessage(`{}`)
		}
		calls = append(calls, provider.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args, Raw: raw})
	}

	stop := normalizeStop(choice.FinishReason)
	if choice.FinishReason == "tool_calls" || len(calls) > 0 {
		stop = "tool_use"
	}

	// Providers report prompt_tokens as the TOTAL input (cached + fresh),
	// but claude-CLI usage semantics keep them separate — the parser sums
	// input + cache_read for the context total. Split accordingly.
	cached := out.Usage.PromptCacheHitTokens
	if cached == 0 {
		cached = out.Usage.PromptTokensDetails.CachedTokens
	}
	input := out.Usage.PromptTokens - cached
	if input < 0 {
		input = 0
	}

	return provider.Result{
		Text:       choice.Message.Content,
		ToolCalls:  calls,
		StopReason: stop,
		Usage: provider.Usage{
			InputTokens:  input,
			CacheRead:    cached,
			OutputTokens: out.Usage.CompletionTokens,
		},
	}, nil
}
