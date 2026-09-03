package session

import (
	"encoding/json"
	"testing"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestLoadMissingSessionIsEmpty(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	msgs, err := st.Load("abc-123")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("want empty history, got %d", len(msgs))
	}
}

func TestAppendThenLoadRoundTrips(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	u := envelope.Msg{Role: "user", Content: []json.RawMessage{raw(`{"type":"text","text":"hai"}`)}}
	a := envelope.Msg{Role: "assistant", Content: []json.RawMessage{raw(`{"type":"text","text":"halo"}`)}}
	if err := st.Append("s1", u, a); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := st.Append("s1", u); err != nil { // second turn appends, not truncates
		t.Fatalf("Append 2: %v", err)
	}
	msgs, err := st.Load("s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("history = %d msgs, want 3", len(msgs))
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}
}

func TestRejectsPathTraversalID(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	if err := st.Append("../evil", envelope.Msg{Role: "user"}); err == nil {
		t.Error("want error for traversal session id")
	}
	if _, err := st.Load("a/b"); err == nil {
		t.Error("want error for slash in session id")
	}
}
