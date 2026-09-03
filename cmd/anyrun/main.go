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
	"strings"

	"github.com/pandeptwidyaop/anyrun/internal/emit"
	"github.com/pandeptwidyaop/anyrun/internal/envelope"
	"github.com/pandeptwidyaop/anyrun/internal/loop"
	"github.com/pandeptwidyaop/anyrun/internal/mcpclient"
	"github.com/pandeptwidyaop/anyrun/internal/provider"
	"github.com/pandeptwidyaop/anyrun/internal/provider/openai"
	"github.com/pandeptwidyaop/anyrun/internal/session"
)

type config struct {
	SessionID        string
	Resume           bool
	SystemPromptFile string
	MCPConfigFile    string
	Model            string
	MaxTurns         int
	Verbose          bool
	DisallowedTools  string // comma-separated globs from --disallowedTools
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
	fs.StringVar(&cfg.DisallowedTools, "disallowedTools", "", "")
	// Accepted for compatibility; anyrun auto-approves everything, so the
	// approve-only --allowedTools flag has nothing to do here.
	fs.String("allowedTools", "", "")
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

	deps := loop.Deps{
		Provider:      &openai.Client{BaseURL: baseURL, APIKey: apiKey},
		Store:         &session.Store{Dir: dir},
		Emit:          w,
		SessionID:     cfg.SessionID,
		Model:         cfg.Model,
		System:        system,
		ContextWindow: ctxWindow,
		MaxTurns:      cfg.MaxTurns,
	}

	ctx := context.Background()
	if cfg.MCPConfigFile != "" {
		mcpCfg, err := mcpclient.LoadConfig(cfg.MCPConfigFile)
		if err != nil {
			return fail(w, err)
		}
		// MCP startup failure is fatal: an agent without its tools must not
		// silently answer toolless.
		pool, err := mcpclient.Start(ctx, mcpCfg)
		if err != nil {
			return fail(w, err)
		}
		defer pool.Close()
		deps.Tools = filterTools(pool.Tools(), cfg.DisallowedTools)
		deps.CallTool = pool.Call
	}

	_ = w.Init(cfg.SessionID, cfg.Model)
	if err := loop.Turn(ctx, deps, userMsg); err != nil {
		return fail(w, err)
	}
	return nil
}

// filterTools drops tools matching --disallowedTools patterns. Only the
// shapes buildArgs produces are supported: exact names and `prefix*` globs.
func filterTools(defs []provider.ToolDef, disallowed string) []provider.ToolDef {
	patterns := []string{}
	for _, p := range strings.Split(disallowed, ",") {
		if p = strings.TrimSpace(p); p != "" {
			patterns = append(patterns, p)
		}
	}
	if len(patterns) == 0 {
		return defs
	}
	var out []provider.ToolDef
	for _, d := range defs {
		blocked := false
		for _, p := range patterns {
			if p == d.Name || (strings.HasSuffix(p, "*") && strings.HasPrefix(d.Name, strings.TrimSuffix(p, "*"))) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, d)
		}
	}
	return out
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
