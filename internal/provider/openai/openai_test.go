package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
)

func fakeServer(t *testing.T, capture *map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth = %q", got)
		}
		json.NewDecoder(r.Body).Decode(capture)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"halo!"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":42,"completion_tokens":7}
		}`))
	}))
}

func TestChatTranslatesAndNormalizes(t *testing.T) {
	var got map[string]any
	srv := fakeServer(t, &got)
	defer srv.Close()

	p := &Client{BaseURL: srv.URL, APIKey: "sk-test"}
	res, err := p.Chat(context.Background(), provider.Request{
		Model:  "test-model",
		System: "you are Sarah",
		Messages: []envelope.Msg{
			{Role: "user", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"hai"}`)}},
			{Role: "assistant", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"ya?"}`)}},
			{Role: "user", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"apa kabar"}`)}},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Text != "halo!" || res.StopReason != "end_turn" ||
		res.Usage.InputTokens != 42 || res.Usage.OutputTokens != 7 {
		t.Errorf("bad result: %+v", res)
	}

	msgs := got["messages"].([]any)
	if len(msgs) != 4 { // system + 3 history
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "you are Sarah" {
		t.Errorf("system message wrong: %v", first)
	}
	last := msgs[3].(map[string]any)
	if last["role"] != "user" || last["content"] != "apa kabar" {
		t.Errorf("last message wrong: %v", last)
	}
}

func TestChatSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid api key"}}`, 401)
	}))
	defer srv.Close()
	p := &Client{BaseURL: srv.URL, APIKey: "bad"}
	_, err := p.Chat(context.Background(), provider.Request{Model: "m",
		Messages: []envelope.Msg{{Role: "user", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"x"}`)}}}})
	if err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("want provider error surfaced, got %v", err)
	}
}
