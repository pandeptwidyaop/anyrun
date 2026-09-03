// Package envelope decodes the single stdin message claude-agent pipes in:
// {"type":"user","message":{"role":"user","content":[<anthropic blocks>]}}.
// Blocks stay raw JSON — anyrun forwards them, it does not interpret them.
package envelope

import (
	"encoding/json"
	"fmt"
	"io"
)

type Msg struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

func Read(r io.Reader) (Msg, error) {
	var env struct {
		Type    string `json:"type"`
		Message Msg    `json:"message"`
	}
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return Msg{}, fmt.Errorf("envelope: decode stdin: %w", err)
	}
	if env.Type != "user" || len(env.Message.Content) == 0 {
		return Msg{}, fmt.Errorf("envelope: want type=user with content, got type=%q", env.Type)
	}
	return env.Message, nil
}
