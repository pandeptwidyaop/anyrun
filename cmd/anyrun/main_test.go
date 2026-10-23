package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pandeptwidyaop/anyrun/internal/builtin"
	"github.com/pandeptwidyaop/anyrun/internal/emit"
	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/loop"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
	"github.com/pandeptwidyaop/anyrun/internal/session"
)

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
	if cfg.DisallowedTools != "Workflow" {
		t.Errorf("disallowedTools not captured: %+v", cfg)
	}
}

func TestCompactionConfig(t *testing.T) {
	clear := func() {
		for _, k := range []string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS", "ANYRUN_CONTEXT_WINDOW",
			"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", "ANYRUN_COMPACT_AT", "DISABLE_AUTO_COMPACT"} {
			t.Setenv(k, "")
		}
	}

	clear()
	w, c := compactionConfig()
	if w != 0 || c != 0.8 {
		t.Errorf("defaults wrong: window=%d compactAt=%v", w, c)
	}

	clear()
	t.Setenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS", "1000000")
	t.Setenv("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", "70")
	w, c = compactionConfig()
	if w != 1000000 || c != 0.7 {
		t.Errorf("CC env not honored: window=%d compactAt=%v", w, c)
	}

	clear()
	t.Setenv("ANYRUN_CONTEXT_WINDOW", "128000")
	t.Setenv("ANYRUN_COMPACT_AT", "0.5")
	w, c = compactionConfig()
	if w != 128000 || c != 0.5 {
		t.Errorf("fallback env not honored: window=%d compactAt=%v", w, c)
	}

	clear()
	t.Setenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS", "1000000")
	t.Setenv("DISABLE_AUTO_COMPACT", "1")
	w, c = compactionConfig()
	if w != 1000000 || c != 0 {
		t.Errorf("kill switch broken: window=%d compactAt=%v", w, c)
	}
}

func TestFilterTools(t *testing.T) {
	defs := []provider.ToolDef{
		{Name: "mcp__agent__memory_persist"},
		{Name: "mcp__agent__knowledge_get"},
		{Name: "mcp__other__thing"},
	}
	out := filterTools(defs, "mcp__agent__*,exact_name")
	if len(out) != 1 || out[0].Name != "mcp__other__thing" {
		t.Errorf("glob filter wrong: %+v", out)
	}
	out = filterTools(defs, "mcp__other__thing")
	if len(out) != 2 {
		t.Errorf("exact filter wrong: %+v", out)
	}
	if got := filterTools(defs, ""); len(got) != 3 {
		t.Errorf("empty filter should pass all: %+v", got)
	}
}

func TestParseArgsRejectsUnknownOutputFormat(t *testing.T) {
	if _, err := parseArgs([]string{"--print", "--output-format", "text", "--session-id", "x"}); err == nil {
		t.Error("want error for unsupported output format")
	}
}

func TestParseArgsRequiresSession(t *testing.T) {
	if _, err := parseArgs([]string{"--print"}); err == nil {
		t.Error("want error when neither --session-id nor --resume given")
	}
}

func TestParseArgsTools(t *testing.T) {
	cfg, err := parseArgs([]string{"--session-id", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tools != nil {
		t.Errorf("absent --tools should leave Tools nil, got %q", *cfg.Tools)
	}

	cfg, err = parseArgs([]string{"--session-id", "x", "--tools", ""})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tools == nil || *cfg.Tools != "" {
		t.Errorf("--tools \"\" should be set and empty, got %v", cfg.Tools)
	}

	cfg, err = parseArgs([]string{"--session-id", "x", "--tools=read_file,list_dir"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tools == nil || *cfg.Tools != "read_file,list_dir" {
		t.Errorf("--tools list not captured: %v", cfg.Tools)
	}
}

func names(defs []provider.ToolDef) string {
	var out []string
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return strings.Join(out, ",")
}

func TestSelectBuiltins(t *testing.T) {
	bi := &builtin.Set{Root: t.TempDir()}
	empty, list := "", " read_file , list_dir,nope"
	cases := []struct {
		name  string
		tools *string
		bash  bool
		want  string
	}{
		{name: "flag absent keeps all file tools", want: "read_file,edit_file,write_file,list_dir"},
		{name: "flag absent with bash", bash: true, want: "read_file,edit_file,write_file,list_dir,bash"},
		{name: "empty disables every built-in", tools: &empty, bash: true, want: ""},
		{name: "list keeps only those", tools: &list, want: "read_file,list_dir"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bi.AllowBash = c.bash
			if got := names(selectBuiltins(bi.Defs(), c.tools)); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}

	// --tools cannot switch bash on by itself: it stays behind --allow-bash.
	bash := "bash,read_file"
	bi.AllowBash = false
	if got := names(selectBuiltins(bi.Defs(), &bash)); got != "read_file" {
		t.Errorf("bash leaked through --tools without --allow-bash: %q", got)
	}
}

// scriptProvider replays canned results, one per Chat call.
type scriptProvider struct{ script []provider.Result }

func (p *scriptProvider) Chat(_ context.Context, _ provider.Request) (provider.Result, error) {
	res := p.script[0]
	if len(p.script) > 1 {
		p.script = p.script[1:]
	}
	return res, nil
}

// runCalls drives one full turn in which the model calls the given tools
// (whether or not they were exposed to it), and returns the stream output.
func runCalls(t *testing.T, call toolCaller, calls ...provider.ToolCall) string {
	t.Helper()
	var out bytes.Buffer
	p := &scriptProvider{script: []provider.Result{
		{StopReason: "tool_use", ToolCalls: calls},
		{Text: "selesai", StopReason: "end_turn"},
	}}
	err := loop.Turn(context.Background(), loop.Deps{
		Provider: p, Store: &session.Store{Dir: t.TempDir()}, Emit: emit.New(&out),
		SessionID: "s", Model: "m", CallTool: call,
	}, envelope.Msg{Role: "user", Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"hai"}`)}})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	return out.String()
}

// A model that calls a tool it was never offered — hallucinated, or steered
// there by an injected message — must be refused, not obeyed: hiding the
// definition alone is not a boundary.
func TestDisallowedBuiltinIsNotExecuted(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	bi := &builtin.Set{Root: root}
	tools := filterTools(selectBuiltins(bi.Defs(), nil), "read_file,write_file")

	var mcpCalled []string
	mcp := func(_ context.Context, name string, _ json.RawMessage) (string, bool, error) {
		mcpCalled = append(mcpCalled, name)
		return "mcp ran", false, nil
	}
	out := runCalls(t, dispatch(tools, bi, mcp),
		provider.ToolCall{ID: "c1", Name: "write_file", Args: json.RawMessage(`{"path":"pwned.txt","content":"x"}`)},
		provider.ToolCall{ID: "c2", Name: "read_file", Args: json.RawMessage(`{"path":"secret.txt"}`)},
		provider.ToolCall{ID: "c3", Name: "mcp__evil__exfil", Args: json.RawMessage(`{}`)},
	)

	if _, err := os.Stat(filepath.Join(root, "pwned.txt")); !os.IsNotExist(err) {
		t.Errorf("disallowed write_file created the file (stat err=%v)", err)
	}
	if strings.Contains(out, "TOPSECRET") {
		t.Errorf("disallowed read_file leaked the file:\n%s", out)
	}
	for _, n := range []string{"write_file", "read_file", "mcp__evil__exfil"} {
		// isErr surfaces as the loop's "ERROR: " prefix.
		if !strings.Contains(out, `ERROR: tool \"`+n+`\" is not available`) {
			t.Errorf("no error refusal for %s:\n%s", n, out)
		}
	}
	if len(mcpCalled) != 0 {
		t.Errorf("unexposed MCP tool reached the server: %v", mcpCalled)
	}
}

func TestExposedToolsStillRun(t *testing.T) {
	root := t.TempDir()
	bi := &builtin.Set{Root: root}
	tools := append(selectBuiltins(bi.Defs(), nil), provider.ToolDef{Name: "mcp__agent__echo"})

	var mcpCalled []string
	mcp := func(_ context.Context, name string, _ json.RawMessage) (string, bool, error) {
		mcpCalled = append(mcpCalled, name)
		return "echo", false, nil
	}
	runCalls(t, dispatch(tools, bi, mcp),
		provider.ToolCall{ID: "c1", Name: "write_file", Args: json.RawMessage(`{"path":"ok.txt","content":"x"}`)},
		provider.ToolCall{ID: "c2", Name: "mcp__agent__echo", Args: json.RawMessage(`{}`)},
	)
	if _, err := os.Stat(filepath.Join(root, "ok.txt")); err != nil {
		t.Errorf("exposed write_file did not run: %v", err)
	}
	if len(mcpCalled) != 1 || mcpCalled[0] != "mcp__agent__echo" {
		t.Errorf("exposed MCP tool not dispatched: %v", mcpCalled)
	}
}

// --tools "" turns every built-in off, yet MCP tools keep working.
func TestToolsEmptyKeepsMCP(t *testing.T) {
	root := t.TempDir()
	bi := &builtin.Set{Root: root}
	empty := ""
	tools := append(selectBuiltins(bi.Defs(), &empty), provider.ToolDef{Name: "mcp__agent__echo"})

	var mcpCalled []string
	mcp := func(_ context.Context, name string, _ json.RawMessage) (string, bool, error) {
		mcpCalled = append(mcpCalled, name)
		return "echo", false, nil
	}
	out := runCalls(t, dispatch(tools, bi, mcp),
		provider.ToolCall{ID: "c1", Name: "list_dir", Args: json.RawMessage(`{"path":"/"}`)},
		provider.ToolCall{ID: "c2", Name: "mcp__agent__echo", Args: json.RawMessage(`{}`)},
	)
	if !strings.Contains(out, `tool \"list_dir\" is not available`) {
		t.Errorf("list_dir not refused:\n%s", out)
	}
	if len(mcpCalled) != 1 {
		t.Errorf("MCP tool should still run: %v", mcpCalled)
	}
}
