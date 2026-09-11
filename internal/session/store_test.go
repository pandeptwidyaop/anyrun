package session

import (
	"encoding/json"
	"os"
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

// A kill during Append leaves a truncated final line. That must not make the
// whole session unreadable — every earlier message is still valid history.
func TestLoadToleratesTornFinalLine(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	u := envelope.Msg{Role: "user", Content: []json.RawMessage{raw(`{"type":"text","text":"hai"}`)}}
	a := envelope.Msg{Role: "assistant", Content: []json.RawMessage{raw(`{"type":"text","text":"halo"}`)}}
	if err := st.Append("s1", u, a); err != nil {
		t.Fatalf("Append: %v", err)
	}

	p, err := st.FilePath("s1")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Half a JSON object, no trailing newline: exactly what a SIGKILL mid-write
	// leaves behind.
	if _, err := f.WriteString(`{"role":"assistant","content":[{"type":"to`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	msgs, err := st.Load("s1")
	if err != nil {
		t.Fatalf("Load must survive a torn tail, got: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("history = %d, want 2 complete messages", len(msgs))
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].Role = %q, want assistant", msgs[1].Role)
	}
}

// Corruption in the middle is not a torn write — it means real damage, and
// silently dropping it would hide that.
func TestLoadRejectsCorruptionInMiddle(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	p, err := st.FilePath("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"role":"user","content":[]}` + "\n" +
		`{"role":"assistant","content":[{"type":"te` + "\n" + // broken, but NOT last
		`{"role":"user","content":[]}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Load("s1"); err == nil {
		t.Error("want error for corruption in the middle of the file")
	}
}

// A complete file whose final line happens to be invalid is corruption, not a
// torn write: the newline proves the whole line was written.
func TestLoadRejectsCorruptFinalLineWhenFileEndsWithNewline(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := st.FilePath("s1")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"role":"user","content":[]}` + "\n" + `{"role":"assistant","content":[{"type":"te` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Load("s1"); err == nil {
		t.Error("want error: the trailing newline shows the line was written in full")
	}
}
