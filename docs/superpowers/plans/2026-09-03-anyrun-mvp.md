# anyrun MVP Implementation Plan (Milestone 1: core turn loop, OpenAI style, no MCP)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A working `anyrun` binary that claude-agent can spawn instead of `claude`: reads the stdin envelope, keeps JSONL sessions, calls one OpenAI-style provider, and emits the stream-json subset `internal/claude/parseStreamLine` consumes.

**Architecture:** Canonical message format is Anthropic-shaped blocks (what the stdin envelope already carries). One non-streaming HTTP call per model message; each completed message is emitted as one `assistant` line (the parser never reads deltas — verified against `runner.go`). Tools/MCP, media translation, and the anthropic/gemini adapters are later milestones.

**Tech Stack:** Go 1.25, stdlib only (net/http, encoding/json, flag). Module `github.com/pandeptwidyaop/anyrun`.

**Spec:** `docs/superpowers/specs/2026-09-03-anyrun-design.md` (read the "Contract corrections" section first).

**Out of scope for this milestone:** MCP bridge, tool-calling loop, media blocks, anthropic/gemini adapters, claude-agent's `runner.go` tweak. Each gets its own plan.

---

### Task 1: Module + envelope package

**Files:**
- Create: `go.mod`
- Create: `internal/envelope/envelope.go`
- Test: `internal/envelope/envelope_test.go`

- [ ] **Step 1: Init module**

```bash
cd /home/devops/projects/anyrun && go mod init github.com/pandeptwidyaop/anyrun
```

- [ ] **Step 2: Write the failing test**

```go
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/envelope/`
Expected: FAIL (package missing).

- [ ] **Step 4: Implement**

```go
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
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/envelope/` — Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add go.mod internal/envelope/
git commit -m "feat: decode the claude-agent stdin envelope"
```

---

### Task 2: Session store (JSONL)

**Files:**
- Create: `internal/session/store.go`
- Test: `internal/session/store_test.go`

- [ ] **Step 1: Write the failing test**

```go
package session

import (
	"encoding/json"
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
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/session/` — Expected: FAIL (package missing).

- [ ] **Step 3: Implement**

```go
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
// so an id can never escape Dir (vidbox gotcha 15: filepath.Base("..") is "..").
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
	for sc.Scan() {
		var m envelope.Msg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/session/` — Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/session/
git commit -m "feat: JSONL session store with full-history replay"
```

---

### Task 3: Event emitter (the output side of the contract)

**Files:**
- Create: `internal/emit/emit.go`
- Test: `internal/emit/emit_test.go`

The shapes below are exactly what `parseStreamLine` in claude-agent reads.
Nothing else may be emitted on stdout — stray prints corrupt the stream.

- [ ] **Step 1: Write the failing test**

```go
package emit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func lines(buf *bytes.Buffer) []string {
	return strings.Split(strings.TrimSpace(buf.String()), "\n")
}

func TestAssistantTextShape(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	if err := w.AssistantText("halo Kak"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if got.Type != "assistant" || len(got.Message.Content) != 1 ||
		got.Message.Content[0].Type != "text" || got.Message.Content[0].Text != "halo Kak" {
		t.Errorf("bad shape: %s", buf.String())
	}
}

func TestResultCarriesUsageAndContextWindow(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	err := w.Result(ResultInfo{
		Text: "done", StopReason: "end_turn", DurationMS: 1234,
		InputTokens: 100, OutputTokens: 20, Model: "gpt-x", ContextWindow: 1000000,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	for _, want := range []string{
		`"type":"result"`, `"result":"done"`, `"stop_reason":"end_turn"`,
		`"duration_ms":1234`, `"input_tokens":100`, `"output_tokens":20`,
		`"contextWindow":1000000`, `"is_error":false`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("result line missing %s\n%s", want, s)
		}
	}
}

func TestErrorResultSetsFlagAndErrors(t *testing.T) {
	var buf bytes.Buffer
	w := New(&buf)
	if err := w.ResultError("auth failed", 50); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, `"is_error":true`) || !strings.Contains(s, "auth failed") {
		t.Errorf("bad error result: %s", s)
	}
}
```

(Note: fix the intentional syntax check — the string list must be valid Go:
`` `"input_tokens":100` `` and `` `"output_tokens":20` `` are separate items.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/emit/` — Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// Package emit writes the stream-json subset claude-agent's parser consumes.
// Contract (internal/claude/runner.go parseStreamLine):
//   assistant  -> message.content[] blocks: text | thinking | tool_use
//   user       -> message.content[] blocks: tool_result
//   result     -> result, stop_reason, duration_ms, total_cost_usd,
//                 is_error, errors[], usage{...}, modelUsage{m:{contextWindow}}
package emit

import (
	"encoding/json"
	"io"
	"sync"
)

type Writer struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func New(w io.Writer) *Writer { return &Writer{enc: json.NewEncoder(w)} }

func (w *Writer) write(v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(v)
}

// Init mirrors the CLI's first line. The parser ignores it, but it is cheap
// insurance for anything else that tails the stream.
func (w *Writer) Init(sessionID, model string) error {
	return w.write(map[string]any{
		"type": "system", "subtype": "init",
		"session_id": sessionID, "model": model,
	})
}

func (w *Writer) AssistantText(text string) error {
	return w.write(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
}

type ResultInfo struct {
	Text          string
	StopReason    string
	DurationMS    int64
	InputTokens   int
	OutputTokens  int
	Model         string
	ContextWindow int // 0 = omit modelUsage
}

func (w *Writer) Result(r ResultInfo) error {
	m := map[string]any{
		"type": "result", "subtype": "success",
		"result":      r.Text,
		"stop_reason": r.StopReason,
		"duration_ms": r.DurationMS,
		// anyrun does not price tokens; claude-agent's models collection owns
		// pricing and computes cost from the usage fields below.
		"total_cost_usd": 0.0,
		"is_error":       false,
		"usage": map[string]any{
			"input_tokens":                r.InputTokens,
			"output_tokens":               r.OutputTokens,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 0,
		},
	}
	if r.ContextWindow > 0 {
		m["modelUsage"] = map[string]any{r.Model: map[string]any{"contextWindow": r.ContextWindow}}
	}
	return w.write(m)
}

func (w *Writer) ResultError(msg string, durationMS int64) error {
	return w.write(map[string]any{
		"type": "result", "subtype": "error",
		"result": msg, "errors": []string{msg},
		"duration_ms": durationMS, "total_cost_usd": 0.0,
		"is_error": true,
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/emit/` — Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/emit/
git commit -m "feat: emit the stream-json subset claude-agent parses"
```

---

### Task 4: OpenAI-style provider adapter

**Files:**
- Create: `internal/provider/provider.go`
- Create: `internal/provider/openai/openai.go`
- Test: `internal/provider/openai/openai_test.go`

- [ ] **Step 1: Define the provider interface** (`internal/provider/provider.go`)

```go
// Package provider defines the adapter boundary. The loop speaks this
// interface only; all API-style translation lives behind it.
package provider

import (
	"context"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
)

type Request struct {
	Model    string
	System   string // merged personality file contents
	Messages []envelope.Msg
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type Result struct {
	Text       string
	StopReason string // normalized: end_turn | max_tokens | stop_sequence
	Usage      Usage
}

type Provider interface {
	Chat(ctx context.Context, req Request) (Result, error)
}
```

- [ ] **Step 2: Write the failing adapter test**

```go
package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
)

func fakeServer(t *testing.T, capture *map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth = %q", got)
		}
		json.NewDecoder(r.Body).Decode(capture)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"halo!"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":42,"completion_tokens":7}
		}`))
	}))
}

func TestChatTranslatesAndNormalizes(t *testing.T) {
	var got map[string]any
	srv := fakeServer(t, &got)
	defer srv.Close()

	p := &Client{BaseURL: srv.URL, APIKey: "sk-test"}
	res, err := p.Chat(context.Background(), provider.Request{
		Model:  "test-model",
		System: "you are Sarah",
		Messages: []envelope.Msg{
			{Role: "user", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"hai"}`)}},
			{Role: "assistant", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"ya?"}`)}},
			{Role: "user", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"apa kabar"}`)}},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if res.Text != "halo!" || res.StopReason != "end_turn" ||
		res.Usage.InputTokens != 42 || res.Usage.OutputTokens != 7 {
		t.Errorf("bad result: %+v", res)
	}

	msgs := got["messages"].([]any)
	if len(msgs) != 4 { // system + 3 history
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "you are Sarah" {
		t.Errorf("system message wrong: %v", first)
	}
	last := msgs[3].(map[string]any)
	if last["role"] != "user" || last["content"] != "apa kabar" {
		t.Errorf("last message wrong: %v", last)
	}
}

func TestChatSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"invalid api key"}}`, 401)
	}))
	defer srv.Close()
	p := &Client{BaseURL: srv.URL, APIKey: "bad"}
	_, err := p.Chat(context.Background(), provider.Request{Model: "m",
		Messages: []envelope.Msg{{Role: "user", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"x"}`)}}}})
	if err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("want provider error surfaced, got %v", err)
	}
}
```

(Add `"strings"` to the test file imports.)

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/provider/...` — Expected: FAIL.

- [ ] **Step 4: Implement** (`internal/provider/openai/openai.go`)

```go
// Package openai adapts the provider interface to OpenAI-style
// /chat/completions APIs (OpenRouter, vLLM, DeepSeek, ...).
//
// Milestone 1 scope: text blocks only, no tools, non-streaming. The
// observable behavior toward claude-agent is identical either way, because
// its parser consumes complete messages, not deltas.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
)

type Client struct {
	BaseURL string // e.g. https://openrouter.ai/api/v1
	APIKey  string
	HTTP    *http.Client
}

type oaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// flattenText joins the text blocks of an Anthropic-shaped message.
// Non-text blocks (image/document) are out of scope in milestone 1 and are
// replaced by a placeholder so the model at least knows something was there.
func flattenText(m envelope.Msg) string {
	var parts []string
	for _, raw := range m.Content {
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		default:
			parts = append(parts, fmt.Sprintf("[unsupported %s block omitted]", b.Type))
		}
	}
	return strings.Join(parts, "\n")
}

func normalizeStop(reason string) string {
	switch reason {
	case "stop", "":
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		return reason
	}
}

func (c *Client) Chat(ctx context.Context, req provider.Request) (provider.Result, error) {
	msgs := make([]oaMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, oaMessage{Role: m.Role, Content: flattenText(m)})
	}

	body, err := json.Marshal(map[string]any{
		"model":    req.Model,
		"messages": msgs,
	})
	if err != nil {
		return provider.Result{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return provider.Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return provider.Result{}, fmt.Errorf("openai: request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return provider.Result{}, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var out struct {
		Choices []struct {
			Message      oaMessage `json:"message"`
			FinishReason string    `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return provider.Result{}, fmt.Errorf("openai: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return provider.Result{}, fmt.Errorf("openai: empty choices: %s", string(raw))
	}
	return provider.Result{
		Text:       out.Choices[0].Message.Content,
		StopReason: normalizeStop(out.Choices[0].FinishReason),
		Usage: provider.Usage{
			InputTokens:  out.Usage.PromptTokens,
			OutputTokens: out.Usage.CompletionTokens,
		},
	}, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/provider/...` — Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/provider/
git commit -m "feat: openai-style provider adapter (text, non-streaming)"
```

---

### Task 5: Turn loop

**Files:**
- Create: `internal/loop/loop.go`
- Test: `internal/loop/loop_test.go`

- [ ] **Step 1: Write the failing test**

```go
package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pandeptwidyaop/anyrun/internal/emit"
	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
	"github.com/pandeptwidyaop/anyrun/internal/session"
)

type fakeProvider struct{ gotMessages int }

func (f *fakeProvider) Chat(_ context.Context, req provider.Request) (provider.Result, error) {
	f.gotMessages = len(req.Messages)
	return provider.Result{Text: "jawaban", StopReason: "end_turn",
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 2}}, nil
}

func userMsg(text string) envelope.Msg {
	return envelope.Msg{Role: "user",
		Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"` + text + `"}`)}}
}

func TestTurnEmitsAssistantAndResultAndPersists(t *testing.T) {
	var out bytes.Buffer
	st := &session.Store{Dir: t.TempDir()}
	fp := &fakeProvider{}

	err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s1", Model: "m", System: "sys",
	}, userMsg("halo"))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}

	s := out.String()
	if !strings.Contains(s, `"type":"assistant"`) || !strings.Contains(s, "jawaban") {
		t.Errorf("missing assistant event:\n%s", s)
	}
	if !strings.Contains(s, `"type":"result"`) || !strings.Contains(s, `"input_tokens":10`) {
		t.Errorf("missing result event:\n%s", s)
	}

	// Second turn must see the persisted history: user+assistant+user = 3.
	if err := Turn(context.Background(), Deps{
		Provider: fp, Store: st, Emit: emit.New(&out),
		SessionID: "s1", Model: "m", System: "sys",
	}, userMsg("lanjut")); err != nil {
		t.Fatal(err)
	}
	if fp.gotMessages != 3 {
		t.Errorf("provider saw %d messages, want 3 (replayed history)", fp.gotMessages)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/loop/` — Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// Package loop runs one conversational turn: load history, call the
// provider, emit contract events, persist. (The agentic multi-turn tool
// loop arrives with the MCP milestone; the seam is already here.)
package loop

import (
	"context"
	"encoding/json"
	"time"

	"github.com/pandeptwidyaop/anyrun/internal/emit"
	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
	"github.com/pandeptwidyaop/anyrun/internal/session"
)

type Deps struct {
	Provider      provider.Provider
	Store         *session.Store
	Emit          *emit.Writer
	SessionID     string
	Model         string
	System        string
	ContextWindow int // from ANYRUN_CONTEXT_WINDOW; 0 = unknown
}

func Turn(ctx context.Context, d Deps, userMsg envelope.Msg) error {
	start := time.Now()

	history, err := d.Store.Load(d.SessionID)
	if err != nil {
		return err
	}
	messages := append(history, userMsg)

	res, err := d.Provider.Chat(ctx, provider.Request{
		Model: d.Model, System: d.System, Messages: messages,
	})
	if err != nil {
		return err
	}

	if err := d.Emit.AssistantText(res.Text); err != nil {
		return err
	}

	assistantMsg := envelope.Msg{
		Role: "assistant",
		Content: []json.RawMessage{mustBlock(res.Text)},
	}
	// Persist AFTER the provider succeeded: a failed call must not leave a
	// user message in history that the model never saw.
	if err := d.Store.Append(d.SessionID, userMsg, assistantMsg); err != nil {
		return err
	}

	return d.Emit.Result(emit.ResultInfo{
		Text:          res.Text,
		StopReason:    res.StopReason,
		DurationMS:    time.Since(start).Milliseconds(),
		InputTokens:   res.Usage.InputTokens,
		OutputTokens:  res.Usage.OutputTokens,
		Model:         d.Model,
		ContextWindow: d.ContextWindow,
	})
}

func mustBlock(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": "text", "text": text})
	return b
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/loop/` — Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/loop/
git commit -m "feat: single-turn loop with history replay and persistence"
```

---

### Task 6: CLI wiring (`cmd/anyrun`)

**Files:**
- Create: `cmd/anyrun/main.go`
- Test: `cmd/anyrun/main_test.go` (flag parsing only; process-level smoke is Task 7)

- [ ] **Step 1: Write the failing flag-parsing test**

```go
package main

import "testing"

func TestParseArgsClaudeAgentShape(t *testing.T) {
	// Exactly what claude-agent's buildArgs produces (order included).
	cfg, err := parseArgs([]string{
		"--print", "--output-format", "stream-json", "--verbose",
		"--resume", "abc-123",
		"--system-prompt-file", "/tmp/p.md",
		"--mcp-config", "/tmp/mcp.json",
		"--model", "google/gemini-2.5-pro",
		"--max-turns", "25",
		"--allowedTools", "mcp__agent__*",
		"--disallowedTools", "Workflow",
		"--dangerously-skip-permissions",
		"--input-format", "stream-json",
	})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if cfg.SessionID != "abc-123" || !cfg.Resume {
		t.Errorf("session parsing wrong: %+v", cfg)
	}
	if cfg.Model != "google/gemini-2.5-pro" || cfg.SystemPromptFile != "/tmp/p.md" {
		t.Errorf("cfg wrong: %+v", cfg)
	}
}

func TestParseArgsRejectsUnknownOutputFormat(t *testing.T) {
	if _, err := parseArgs([]string{"--print", "--output-format", "text"}); err == nil {
		t.Error("want error for unsupported output format")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/anyrun/` — Expected: FAIL.

- [ ] **Step 3: Implement**

```go
// anyrun: a claude-CLI-compatible runner for non-Claude models.
// It implements the flag/stdin/stdout subset that claude-agent's
// internal/claude package uses — nothing more.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/pandeptwidyaop/anyrun/internal/emit"
	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/loop"
	"github.com/pandeptwidyaop/anyrun/internal/provider/openai"
	"github.com/pandeptwidyaop/anyrun/internal/session"
)

type config struct {
	SessionID        string
	Resume           bool
	SystemPromptFile string
	MCPConfigFile    string // parsed now, used in the MCP milestone
	Model            string
	MaxTurns         int
	Verbose          bool
}

func parseArgs(args []string) (config, error) {
	fs := flag.NewFlagSet("anyrun", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var cfg config
	var outputFormat, inputFormat, sessionID, resumeID string
	fs.Bool("print", true, "")
	fs.StringVar(&outputFormat, "output-format", "stream-json", "")
	fs.StringVar(&inputFormat, "input-format", "stream-json", "")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "")
	fs.StringVar(&sessionID, "session-id", "", "")
	fs.StringVar(&resumeID, "resume", "", "")
	fs.StringVar(&cfg.SystemPromptFile, "system-prompt-file", "", "")
	fs.StringVar(&cfg.MCPConfigFile, "mcp-config", "", "")
	fs.StringVar(&cfg.Model, "model", "", "")
	fs.IntVar(&cfg.MaxTurns, "max-turns", 0, "")
	// Accepted for compatibility, intentionally unused in this milestone.
	fs.String("allowedTools", "", "")
	fs.String("disallowedTools", "", "")
	fs.Bool("dangerously-skip-permissions", false, "")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if outputFormat != "stream-json" || inputFormat != "stream-json" {
		return config{}, fmt.Errorf("anyrun: only stream-json in/out is supported")
	}
	if resumeID != "" {
		cfg.SessionID, cfg.Resume = resumeID, true
	} else {
		cfg.SessionID = sessionID
	}
	if cfg.SessionID == "" {
		return config{}, fmt.Errorf("anyrun: --session-id or --resume is required")
	}
	return cfg, nil
}

func run() error {
	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		return err
	}

	w := emit.New(os.Stdout)

	style := os.Getenv("ANYRUN_API_STYLE")
	if style == "" {
		style = "openai"
	}
	if style != "openai" {
		return fail(w, fmt.Errorf("anyrun: API style %q not implemented yet", style))
	}
	baseURL, apiKey := os.Getenv("ANYRUN_BASE_URL"), os.Getenv("ANYRUN_API_KEY")
	if baseURL == "" || apiKey == "" || cfg.Model == "" {
		return fail(w, fmt.Errorf("anyrun: ANYRUN_BASE_URL, ANYRUN_API_KEY and --model are required"))
	}

	system := ""
	if cfg.SystemPromptFile != "" {
		b, err := os.ReadFile(cfg.SystemPromptFile)
		if err != nil {
			return fail(w, err)
		}
		system = string(b)
	}

	userMsg, err := envelope.Read(os.Stdin)
	if err != nil {
		return fail(w, err)
	}

	dir := os.Getenv("ANYRUN_SESSION_DIR")
	if dir == "" {
		if dir, err = session.DefaultDir(); err != nil {
			return fail(w, err)
		}
	}

	ctxWindow, _ := strconv.Atoi(os.Getenv("ANYRUN_CONTEXT_WINDOW"))

	_ = w.Init(cfg.SessionID, cfg.Model)
	err = loop.Turn(context.Background(), loop.Deps{
		Provider:      &openai.Client{BaseURL: baseURL, APIKey: apiKey},
		Store:         &session.Store{Dir: dir},
		Emit:          w,
		SessionID:     cfg.SessionID,
		Model:         cfg.Model,
		System:        system,
		ContextWindow: ctxWindow,
	}, userMsg)
	if err != nil {
		return fail(w, err)
	}
	return nil
}

// fail mirrors CLI behavior: error result event on stdout, message on
// stderr, non-zero exit — the worker handles all three already.
func fail(w *emit.Writer, err error) error {
	_ = w.ResultError(err.Error(), 0)
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Run all tests + build**

Run: `go test ./... && go build ./cmd/anyrun && go vet ./...`
Expected: PASS, binary `anyrun` builds.

- [ ] **Step 5: Commit**

```bash
git add cmd/
git commit -m "feat: anyrun CLI wiring with claude-compatible flags"
```

---

### Task 7: End-to-end smoke + push

**Files:** none new

- [ ] **Step 1: Process-level smoke against a fake server**

Start a fake OpenAI server and run the real binary twice (create + resume):

```bash
cd /home/devops/projects/anyrun
cat > /tmp/anyrun-fake.go <<'EOF'
package main

import ("fmt"; "net/http")

func main() {
	http.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	})
	http.ListenAndServe("127.0.0.1:8199", nil)
}
EOF
go run /tmp/anyrun-fake.go &
sleep 1
export ANYRUN_BASE_URL=http://127.0.0.1:8199 ANYRUN_API_KEY=x ANYRUN_SESSION_DIR=/tmp/anyrun-smoke
echo '{"type":"user","message":{"role":"user","content":[{"type":"text","text":"ping"}]}}' \
  | ./anyrun --print --output-format stream-json --input-format stream-json \
      --session-id 11111111-1111-1111-1111-111111111111 --model test
echo '{"type":"user","message":{"role":"user","content":[{"type":"text","text":"lagi"}]}}' \
  | ./anyrun --print --output-format stream-json --input-format stream-json \
      --resume 11111111-1111-1111-1111-111111111111 --model test
kill %1
wc -l /tmp/anyrun-smoke/11111111-1111-1111-1111-111111111111.jsonl
```

Expected: each run prints a `system/init`, an `assistant` line containing
"pong", and a `result` line with `"input_tokens":5`; the session file has 4
lines after the second run.

- [ ] **Step 2: Real-provider smoke (env-gated, needs OpenRouter key)**

Same two commands with `ANYRUN_BASE_URL=https://openrouter.ai/api/v1`,
`ANYRUN_API_KEY` from Kak Pande, `--model` a cheap model. Expected: sane
answer text, non-zero usage numbers.

- [ ] **Step 3: Push**

```bash
git push origin main
```

(Confirm with Kak Pande before pushing — house rule.)

---

## Follow-up milestones (separate plans, in order)

1. **MCP bridge + agentic tool loop** — mcp-go client pool, tool schema
   translation, `tool_use`/`tool_result` events, `--max-turns` honored.
   This is the milestone that makes Sarah able to live on anyrun.
2. **claude-agent integration** — `runner.go` prefers
   `cfg.AgentEnv["CLAUDE_BIN"]`; register a test model with
   `CLAUDE_BIN=<path>/anyrun` + `ANYRUN_*` env in the models collection.
3. **anthropic + gemini adapters** — same Provider interface.
4. **Media blocks** — image/document translation per style.
