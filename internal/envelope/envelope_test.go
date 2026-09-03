package envelope

import (
	"strings"
	"testing"
)

func TestReadTextEnvelope(t *testing.T) {
	in := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"halo"}]}}`
	msg, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if msg.Role != "user" {
		t.Errorf("role = %q, want user", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(msg.Content))
	}
	if !strings.Contains(string(msg.Content[0]), "halo") {
		t.Errorf("block lost text: %s", msg.Content[0])
	}
}

func TestReadRejectsGarbage(t *testing.T) {
	if _, err := Read(strings.NewReader("not json")); err == nil {
		t.Error("want error for non-JSON stdin")
	}
	if _, err := Read(strings.NewReader(`{"type":"nope"}`)); err == nil {
		t.Error("want error for wrong envelope type")
	}
}
