package emit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
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
