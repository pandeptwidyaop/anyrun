# anyrun Milestone: Session Compaction

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a session's context grows past a threshold, anyrun summarizes the older history with the same model, keeps the recent tail, and emits a `compact_boundary` event — claude-agent's parser and compaction tracker already understand that event, so the backend needs zero changes.

**Why:** Without provider prompt caching, every turn replays full history: cost grows linearly and eventually overflows the model's window. Claude CLI auto-compacts; anyrun currently surfaces the overflow error and stops there.

**Design decisions (settled):**
- Trigger check happens at the START of a turn using the context size
  recorded at the END of the previous turn (provider usage is the source of
  truth — no client-side token guessing).
- Threshold: `ANYRUN_COMPACT_AT` (0..1, default `0.8`) ×
  `ANYRUN_CONTEXT_WINDOW`. If the window is unset (0), compaction is
  disabled — no window, no arithmetic.
- Keep-tail rule: retain the most recent ~25% of messages, but the cut
  must land so the tail starts on a **user message whose first block is
  text** (never split a tool_use from its tool_result pair).
- The summary becomes the first message of the new history: a user message
  `[Ringkasan percakapan sebelumnya]\n<summary>`.
- JSONL rewrite is atomic (temp file + rename) and the old file is kept
  once as `<id>.jsonl.pre-compact` for manual recovery.
- Per-session meta lives in `<id>.meta.json`: `{"context_tokens":N,"compact_count":M}`.
- Event: `{"type":"compact_boundary","result":"<summary>"}` — matches
  `parseStreamLine`'s fallback path (top-level `result` as summary).
- The summarization call's usage is added to the turn's reported totals.

---

### Task 1: session meta

**Files:** `internal/session/meta.go` + test

```go
type Meta struct {
	ContextTokens int `json:"context_tokens"`
	CompactCount  int `json:"compact_count"`
}
func (s *Store) LoadMeta(id string) (Meta, error)   // missing file => zero Meta
func (s *Store) SaveMeta(id string, m Meta) error   // 0600, same dir
```

Tests: round trip, missing file, invalid id rejected (reuse `idRe`).

- [ ] red → green → commit `feat: per-session context/compaction metadata`

---

### Task 2: compactor

**Files:** `internal/compact/compact.go` + test

```go
// CutIndex returns the index where the kept tail begins, honoring the
// user-text-boundary rule. Returns 0 (no compaction possible) when no
// valid cut exists.
func CutIndex(msgs []envelope.Msg, keepRatio float64) int

// Summarize asks the provider (no tools) for a handoff summary of msgs[:cut].
const summaryPrompt = `Summarize this conversation for a seamless handoff.
Preserve: facts, names, decisions, unresolved tasks, user preferences,
emotional tone, and any commitments made. Be dense but complete. Reply
with the summary only.`
func Summarize(ctx context.Context, p provider.Provider, model string, head []envelope.Msg) (string, provider.Usage, error)

// Rewrite atomically replaces the session file with summary-msg + tail,
// backing up the original to <id>.jsonl.pre-compact.
func (…) Rewrite(store *session.Store, id, summary string, tail []envelope.Msg) error
```

Cut rule test cases: plain text history cuts at ~75%; history where the
75% point lands on a tool_result shifts forward to the next user-text
message; all-tool tail returns 0; tiny histories (<4 msgs) return 0.

- [ ] red → green → commit `feat: history compactor with tool-pair-safe cut`

---

### Task 3: loop integration

**Files:** `internal/loop/loop.go` + test

At the start of `Turn` (before building `msgs`):

```go
if d.ContextWindow > 0 && d.CompactAt > 0 {
	meta, _ := d.Store.LoadMeta(d.SessionID)
	if meta.ContextTokens > int(float64(d.ContextWindow)*d.CompactAt) {
		cut := compact.CutIndex(history, 0.25)
		if cut > 0 {
			summary, u, err := compact.Summarize(ctx, d.Provider, d.Model, history[:cut])
			if err == nil {
				compact.Rewrite(d.Store, d.SessionID, summary, history[cut:])
				history, _ = d.Store.Load(d.SessionID)
				d.Emit.CompactBoundary(summary)
				meta.CompactCount++
				total.InputTokens += u.InputTokens; total.OutputTokens += u.OutputTokens
			} // on error: log to stderr, continue uncompacted — better a big
			  // context than a lost conversation
		}
	}
}
```

At the end of a successful turn: `SaveMeta(id, Meta{ContextTokens: last
call's InputTokens+OutputTokens, CompactCount: meta.CompactCount})`.

New emit method:

```go
func (w *Writer) CompactBoundary(summary string) error {
	return w.write(map[string]any{"type": "compact_boundary", "result": summary})
}
```

Deps additions: `CompactAt float64`.

Tests: scripted provider; meta above threshold triggers summarize + rewrite
+ compact_boundary event + compact_count increment; below threshold
untouched; summarize failure falls through gracefully.

- [ ] red → green → commit `feat: auto-compaction in the turn loop`

---

### Task 4: wiring + smoke + push

- [ ] `cmd/anyrun/main.go`: parse `ANYRUN_COMPACT_AT` (default 0.8, clamp
  0..1), pass to Deps. Invalid value → default + stderr note.
- [ ] Smoke (fake provider): session with fat fake meta + long history →
  next turn emits compact_boundary, file shrinks, `.pre-compact` backup
  exists, resume still coherent.
- [ ] Real smoke (DeepSeek flash): set `ANYRUN_CONTEXT_WINDOW=4000`,
  `ANYRUN_COMPACT_AT=0.5`, have a few turns until compaction fires; verify
  the summary actually carries earlier facts (ask "tadi aku bilang apa?").
- [ ] `go test ./... && go vet ./...`, push, update `anyrun-project`
  knowledge (status + env vars baru).

---

## Out of scope

Cache-aware thresholds, per-model compaction prompts, backend-side
rollover, compaction for the Claude CLI driver (it has its own).
