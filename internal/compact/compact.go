// Package compact shrinks a session's history: the older part is
// summarized by the same model, the recent tail is kept verbatim.
package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// Back the previous content up by COPYING it, never by moving it: renaming
	// the live file away leaves a window with no session file at all, and a
	// process killed inside that window loses the whole history — the one
	// failure here that is unrecoverable rather than merely inconvenient.
	if err := copyFile(old, old+".pre-compact"); err != nil {
		os.Remove(tmp)
		return err
	}
	// Rename is atomic on POSIX: the file is either the old content or the new
	// one, never absent and never partial.
	return os.Rename(tmp, old)
}

// copyFile duplicates src to dst with 0600. A missing src is not an error:
// there is nothing to preserve, and the rewrite is still valid.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
