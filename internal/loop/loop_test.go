package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pandeptwidyaop/anyrun/internal/emit"
	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
	"github.com/pandeptwidyaop/anyrun/internal/session"
)

type fakeProvider struct{ gotMessages int }

func (f *fakeProvider) Chat(_ context.Context, req provider.Request) (provider.Result, error) {
	f.gotMessages = len(req.Messages)
	return provider.Result{Text: "jawaban", StopReason: "end_turn",
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 2}}, nil
}

func userMsg(text string) envelope.Msg {
	return envelope.Msg{Role: "user",
		Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"` + text + `"}`)}}
}

func TestTurnEmitsAssistantAndResultAndPersists(t *testing.T) {
	var out bytes.Buffer
	st := &session.Store{Dir: t.TempDir()}
	fp := &fakeProvider{}

	err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s1", Model: "m", System: "sys",
	}, userMsg("halo"))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	s := out.String()
	if !strings.Contains(s, `"type":"assistant"`) || !strings.Contains(s, "jawaban") {
		t.Errorf("missing assistant event:\n%s", s)
	}
	if !strings.Contains(s, `"type":"result"`) || !strings.Contains(s, `"input_tokens":10`) {
		t.Errorf("missing result event:\n%s", s)
	}

	// Second turn must see the persisted history: user+assistant+user = 3.
	if err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s1", Model: "m", System: "sys",
	}, userMsg("lanjut")); err != nil {
		t.Fatal(err)
	}
	if fp.gotMessages != 3 {
		t.Errorf("provider saw %d messages, want 3 (replayed history)", fp.gotMessages)
	}
}
