// Package compact shrinks a session's history: the older part is
// summarized by the same model, the recent tail is kept verbatim.
package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
	"github.com/pandeptwidyaop/anyrun/internal/session"
)

const summaryPrompt = `Summarize this conversation for a seamless handoff to another assistant instance.
Preserve: facts, names, decisions, unresolved tasks, user preferences, emotional tone, and any commitments made.
Be dense but complete. Reply with the summary only, in the conversation's dominant language.`

// firstBlockType decodes the type of a message's first content block.
func firstBlockType(m envelope.Msg) string {
	if len(m.Content) == 0 {
		return ""
	}
	var b struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(m.Content[0], &b) != nil {
		return ""
	}
	return b.Type
}

// CutIndex returns the index where the kept tail begins. The tail is
// roughly the most recent keepRatio of messages, but the cut is shifted
// forward until it lands on a user message whose first block is text, so a
// tool_use is never separated from its tool_result. Returns 0 when the
// history is too small or no valid cut exists.
func CutIndex(msgs []envelope.Msg, keepRatio float64) int {
	if len(msgs) < 4 {
		return 0
	}
	target := int(float64(len(msgs)) * (1 - keepRatio))
	if target < 1 {
		target = 1
	}
	for i := target; i < len(msgs); i++ {
		if msgs[i].Role == "user" && firstBlockType(msgs[i]) == "text" {
			return i
		}
	}
	return 0
}

// Summarize asks the provider (no tools) for a handoff summary of head.
func Summarize(ctx context.Context, p provider.Provider, model string, head []envelope.Msg) (string, provider.Usage, error) {
	instr, _ := json.Marshal(map[string]string{"type": "text", "text": summaryPrompt})
	msgs := append(append([]envelope.Msg{}, head...), envelope.Msg{
		Role:    "user",
		Content: []json.RawMessage{instr},
	})
	res, err := p.Chat(ctx, provider.Request{Model: model, Messages: msgs})
	if err != nil {
		return "", provider.Usage{}, err
	}
	if res.Text == "" {
		return "", res.Usage, fmt.Errorf("compact: empty summary")
	}
	return res.Text, res.Usage, nil
}

// Rewrite atomically replaces the session file with a summary message
// followed by tail. The original file is kept as <id>.jsonl.pre-compact.
func Rewrite(st *session.Store, id, summary string, tail []envelope.Msg) error {
	old, err := st.FilePath(id)
	if err != nil {
		return err
	}

	sum, _ := json.Marshal(map[string]string{
		"type": "text",
		"text": "[Ringkasan percakapan sebelumnya]\n" + summary,
	})
	newMsgs := append([]envelope.Msg{{Role: "user", Content: []json.RawMessage{sum}}}, tail...)

	tmp := old + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	for _, m := range newMsgs {
		b, err := json.Marshal(m)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Keep one backup for manual recovery, then swap atomically.
	if err := os.Rename(old, old+".pre-compact"); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, old)
}
