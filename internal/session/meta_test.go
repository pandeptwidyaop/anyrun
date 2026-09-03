package session

import "testing"

func TestMetaMissingFileIsZero(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	m, err := st.LoadMeta("abc")
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if m.ContextTokens != 0 || m.CompactCount != 0 {
		t.Errorf("want zero meta, got %+v", m)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	if err := st.SaveMeta("s1", Meta{ContextTokens: 12345, CompactCount: 2}); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	m, err := st.LoadMeta("s1")
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if m.ContextTokens != 12345 || m.CompactCount != 2 {
		t.Errorf("round trip lost data: %+v", m)
	}
}

func TestMetaRejectsBadID(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	if err := st.SaveMeta("../evil", Meta{}); err == nil {
		t.Error("want error for traversal id")
	}
	if _, err := st.LoadMeta("a/b"); err == nil {
		t.Error("want error for slash id")
	}
}
