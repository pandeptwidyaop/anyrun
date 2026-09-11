package compact

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
	"github.com/pandeptwidyaop/anyrun/internal/session"
)

func text(role, s string) envelope.Msg {
	b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
	return envelope.Msg{Role: role, Content: []json.RawMessage{b}}
}

func toolResult(id string) envelope.Msg {
	b, _ := json.Marshal(map[string]any{"type": "tool_result", "tool_use_id": id, "content": "x"})
	return envelope.Msg{Role: "user", Content: []json.RawMessage{b}}
}

func plainHistory(n int) []envelope.Msg {
	var out []envelope.Msg
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			out = append(out, text("user", "tanya"))
		} else {
			out = append(out, text("assistant", "jawab"))
		}
	}
	return out
}

func TestCutIndexPlainHistory(t *testing.T) {
	msgs := plainHistory(20)
	cut := CutIndex(msgs, 0.25)
	if cut <= 0 || cut >= len(msgs) {
		t.Fatalf("cut = %d out of range", cut)
	}
	// tail starts on a user text message
	if msgs[cut].Role != "user" {
		t.Errorf("tail starts on %s, want user", msgs[cut].Role)
	}
	// roughly keeps the last quarter
	if cut < 10 {
		t.Errorf("cut = %d, keeps too much", cut)
	}
}

func TestCutIndexSkipsToolResultBoundary(t *testing.T) {
	// Build history where the natural cut lands on a tool_result.
	msgs := plainHistory(12)
	msgs = append(msgs, text("assistant", "manggil tool")) // 12
	msgs = append(msgs, toolResult("c1"))                  // 13 — natural cut area
	msgs = append(msgs, text("assistant", "hasil"))        // 14
	msgs = append(msgs, text("user", "lanjut"))            // 15
	msgs = append(msgs, text("assistant", "ok"))           // 16

	cut := CutIndex(msgs, 0.25)
	if cut == 0 {
		t.Fatal("no cut found")
	}
	if msgs[cut].Role != "user" {
		t.Fatalf("tail starts on %s", msgs[cut].Role)
	}
	var b struct {
		Type string `json:"type"`
	}
	json.Unmarshal(msgs[cut].Content[0], &b)
	if b.Type != "text" {
		t.Errorf("tail starts on %s block, want text", b.Type)
	}
}

func TestCutIndexTinyHistory(t *testing.T) {
	if cut := CutIndex(plainHistory(3), 0.25); cut != 0 {
		t.Errorf("tiny history should not compact, cut=%d", cut)
	}
}

type fakeSummarizer struct{ gotMsgs int }

func (f *fakeSummarizer) Chat(_ context.Context, req provider.Request) (provider.Result, error) {
	f.gotMsgs = len(req.Messages)
	return provider.Result{Text: "RINGKASAN: obrolan panjang", StopReason: "end_turn",
		Usage: provider.Usage{InputTokens: 100, OutputTokens: 20}}, nil
}

func TestSummarizeAndRewrite(t *testing.T) {
	fp := &fakeSummarizer{}
	msgs := plainHistory(20)
	summary, u, err := Summarize(context.Background(), fp, "m", msgs[:16])
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !strings.Contains(summary, "RINGKASAN") || u.InputTokens != 100 {
		t.Errorf("bad summary/usage: %q %+v", summary, u)
	}
	// summarizer sees head + the instruction message
	if fp.gotMsgs != 17 {
		t.Errorf("summarizer saw %d msgs, want 17", fp.gotMsgs)
	}

	st := &session.Store{Dir: t.TempDir()}
	if err := st.Append("s1", msgs...); err != nil {
		t.Fatal(err)
	}
	if err := Rewrite(st, "s1", summary, msgs[16:]); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	got, err := st.Load("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 { // summary + 4 tail
		t.Fatalf("rewritten history = %d msgs, want 5", len(got))
	}
	if got[0].Role != "user" || !strings.Contains(string(got[0].Content[0]), "RINGKASAN") {
		t.Errorf("summary message wrong: %+v", got[0])
	}
	// backup exists
	if _, err := st.Load("s1"); err != nil {
		t.Fatal(err)
	}
}

// The session file must never be absent, not even momentarily. The previous
// implementation renamed the live file away before putting the new one in
// place: a kill inside that window lost the entire history, and nothing else
// on disk could rebuild it.
func TestRewriteKeepsLiveFileAndBacksUpOldContent(t *testing.T) {
	st := &session.Store{Dir: t.TempDir()}
	msgs := plainHistory(20)
	if err := st.Append("s1", msgs...); err != nil {
		t.Fatal(err)
	}
	before, err := st.Load("s1")
	if err != nil {
		t.Fatal(err)
	}

	if err := Rewrite(st, "s1", "RINGKASAN", msgs[16:]); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}

	// Live file holds the new, shorter history.
	after, err := st.Load("s1")
	if err != nil {
		t.Fatalf("live session file unreadable after Rewrite: %v", err)
	}
	if len(after) != 5 {
		t.Errorf("live history = %d, want 5", len(after))
	}

	// Backup holds the PREVIOUS content — that is what makes it a backup.
	p, err := st.FilePath("s1")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(p + ".pre-compact")
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	lines := strings.Count(strings.TrimSpace(string(backup)), "\n") + 1
	if lines != len(before) {
		t.Errorf("backup has %d lines, want the previous %d", lines, len(before))
	}
}

// A Rewrite that fails before the atomic swap must leave the old history intact.
func TestRewriteFailureLeavesOldHistoryIntact(t *testing.T) {
	st := &session.Store{Dir: t.TempDir()}
	msgs := plainHistory(20)
	if err := st.Append("s1", msgs...); err != nil {
		t.Fatal(err)
	}
	p, err := st.FilePath("s1")
	if err != nil {
		t.Fatal(err)
	}

	// Make the backup step fail: .pre-compact exists as a directory, so
	// opening it as a file cannot succeed.
	if err := os.MkdirAll(p+".pre-compact", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Rewrite(st, "s1", "RINGKASAN", msgs[16:]); err == nil {
		t.Fatal("want error when the backup cannot be written")
	}

	got, err := st.Load("s1")
	if err != nil {
		t.Fatalf("old history must survive a failed Rewrite: %v", err)
	}
	if len(got) != len(msgs) {
		t.Errorf("history = %d, want the original %d", len(got), len(msgs))
	}
}
