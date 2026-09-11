// Package session persists conversations as one JSONL file per session id.
// Every line is an envelope.Msg. anyrun replays the whole file each turn —
// full-context fidelity is the whole point, so there is no truncation here.
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
)

// Session ids come from claude-agent as UUIDs; anything fancier is refused
// so an id can never escape Dir (filepath.Base("..") is still "..").
var idRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

type Store struct {
	Dir string // e.g. ~/.anyrun/sessions
}

func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".anyrun", "sessions"), nil
}

func (s *Store) path(id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", fmt.Errorf("session: invalid id %q", id)
	}
	return filepath.Join(s.Dir, id+".jsonl"), nil
}

// FilePath exposes the JSONL path for a session (used by the compactor's
// atomic rewrite). Same id validation as every other entry point.
func (s *Store) FilePath(id string) (string, error) {
	return s.path(id)
}

// endsWithNewline reports whether the file's final byte is '\n'. Append writes
// one complete line per message, so a file that does not end in '\n' can only
// mean the last write was cut short by a kill.
func endsWithNewline(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return true // nothing written, nothing torn
	}
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, st.Size()-1); err != nil {
		return true // unreadable tail: let the scanner report the real problem
	}
	return buf[0] == '\n'
}

func (s *Store) Load(id string) ([]envelope.Msg, error) {
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var msgs []envelope.Msg
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // media blocks can be huge
	complete := endsWithNewline(f)
	for sc.Scan() {
		var m envelope.Msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			// A kill during Append truncates the final line, leaving bytes
			// that cannot parse. Only a torn LAST line is forgiven; a broken
			// line anywhere else is real corruption and must surface.
			if !complete && !sc.Scan() {
				fmt.Fprintf(os.Stderr, "session %s: dropping torn final line: %v\n", id, err)
				break
			}
			return nil, fmt.Errorf("session %s: corrupt line: %w", id, err)
		}
		msgs = append(msgs, m)
	}
	return msgs, sc.Err()
}

func (s *Store) Append(id string, msgs ...envelope.Msg) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return nil
}
