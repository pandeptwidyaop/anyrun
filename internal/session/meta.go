package session

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
)

// Meta is per-session bookkeeping for compaction. ContextTokens is the
// context size reported by the provider at the end of the last turn —
// the source of truth for the next turn's compaction decision.
type Meta struct {
	ContextTokens int `json:"context_tokens"`
	CompactCount  int `json:"compact_count"`
}

func (s *Store) metaPath(id string) (string, error) {
	p, err := s.path(id)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(p, ".jsonl") + ".meta.json", nil
}

func (s *Store) LoadMeta(id string) (Meta, error) {
	p, err := s.metaPath(id)
	if err != nil {
		return Meta{}, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, nil
	}
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		// A corrupt meta file only degrades compaction timing; start fresh.
		return Meta{}, nil
	}
	return m, nil
}

func (s *Store) SaveMeta(id string, m Meta) error {
	p, err := s.metaPath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}
