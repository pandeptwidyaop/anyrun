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
