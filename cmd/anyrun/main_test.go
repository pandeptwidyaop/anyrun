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
	if _, err := parseArgs([]string{"--print", "--output-format", "text", "--session-id", "x"}); err == nil {
		t.Error("want error for unsupported output format")
	}
}

func TestParseArgsRequiresSession(t *testing.T) {
	if _, err := parseArgs([]string{"--print"}); err == nil {
		t.Error("want error when neither --session-id nor --resume given")
	}
}
