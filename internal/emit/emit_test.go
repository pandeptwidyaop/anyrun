package emit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pandeptwidyaop/anyrun/internal/provider"
)

func TestAssistantTextShape(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	if err := w.AssistantText("halo Kak"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if got.Type != "assistant" || len(got.Message.Content) != 1 ||
		got.Message.Content[0].Type != "text" || got.Message.Content[0].Text != "halo Kak" {
		t.Errorf("bad shape: %s", buf.String())
	}
}

func TestAssistantTurnToolUseShape(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	err := w.AssistantTurn("mikir dulu", []provider.ToolCall{
		{ID: "call_1", Name: "mcp__agent__echo", Args: json.RawMessage(`{"msg":"hai"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if len(got.Message.Content) != 2 {
		t.Fatalf("blocks = %d, want 2", len(got.Message.Content))
	}
	tu := got.Message.Content[1]
	if tu.Type != "tool_use" || tu.ID != "call_1" || tu.Name != "mcp__agent__echo" {
		t.Errorf("bad tool_use: %+v", tu)
	}
	// input must be a JSON object, not a quoted string
	if !strings.HasPrefix(strings.TrimSpace(string(tu.Input)), "{") {
		t.Errorf("input not an object: %s", tu.Input)
	}
}

func TestToolResultsShape(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	if err := w.ToolResults([]ToolResult{{ID: "call_1", Content: "echo: hai"}}); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	for _, want := range []string{`"type":"user"`, `"type":"tool_result"`, `"tool_use_id":"call_1"`, `"content":"echo: hai"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
}

func TestResultCarriesUsageAndContextWindow(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	err := w.Result(ResultInfo{
		Text: "done", StopReason: "end_turn", DurationMS: 1234,
		InputTokens: 100, OutputTokens: 20, Model: "gpt-x", ContextWindow: 1000000,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	for _, want := range []string{
		`"type":"result"`, `"result":"done"`, `"stop_reason":"end_turn"`,
		`"duration_ms":1234`, `"input_tokens":100`, `"output_tokens":20`,
		`"contextWindow":1000000`, `"is_error":false`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("result line missing %s\n%s", want, s)
		}
	}
}

func TestErrorResultSetsFlagAndErrors(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	if err := w.ResultError("auth failed", 50); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, `"is_error":true`) || !strings.Contains(s, "auth failed") {
		t.Errorf("bad error result: %s", s)
	}
}

// Compaction is the one pause the caller cannot infer from the stream, so it
// is announced before the work and marked when it lands.
func TestCompactEvents(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	if err := w.CompactStart(120_000, 200_000); err != nil {
		t.Fatalf("CompactStart: %v", err)
	}
	if err := w.CompactBoundary("ringkasan", 120_000); err != nil {
		t.Fatalf("CompactBoundary: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	var start map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &start); err != nil {
		t.Fatalf("start line: %v", err)
	}
	if start["type"] != "system" || start["subtype"] != "compact_start" {
		t.Errorf("start = %v, want system/compact_start", start)
	}
	if start["pre_tokens"] != float64(120_000) || start["context_window"] != float64(200_000) {
		t.Errorf("start carries no sizes: %v", start)
	}
	var done map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &done); err != nil {
		t.Fatalf("boundary line: %v", err)
	}
	if done["type"] != "compact_boundary" || done["result"] != "ringkasan" || done["pre_tokens"] != float64(120_000) {
		t.Errorf("boundary = %v", done)
	}
}
