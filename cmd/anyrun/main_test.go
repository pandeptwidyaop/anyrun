package main

import (
	"testing"

	"github.com/pandeptwidyaop/anyrun/internal/provider"
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
