// Package builtin gives anyrun its own file tools, so it can work on a
// codebase without an MCP server in front of it. The four file tools are
// always on; bash is opt-in (--allow-bash) because a host that already
// brokers shell access screens the commands it runs, and a second,
// unscreened path through anyrun would quietly bypass those rules.
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pandeptwidyaop/anyrun/internal/provider"
)

const (
	maxRead   = 200_000 // bytes returned by read_file before truncation
	maxOutput = 100_000 // bytes returned by bash before truncation
)

// Set is the tool group bound to one working directory.
type Set struct {
	Root      string // relative paths resolve against this
	AllowBash bool
}

// Defs lists the tools in the shape the provider expects.
func (s *Set) Defs() []provider.ToolDef {
	defs := []provider.ToolDef{
		{
			Name:        "read_file",
			Description: "Read a text file. Use offset/limit (1-based line numbers) to page through a file too large to read at once.",
			Schema: json.RawMessage(`{"type":"object","properties":{
				"path":{"type":"string","description":"absolute, or relative to the working directory"},
				"offset":{"type":"number","description":"first line to return (1-based)"},
				"limit":{"type":"number","description":"how many lines to return"}},
				"required":["path"]}`),
		},
		{
			Name:        "edit_file",
			Description: "Replace an exact snippet inside a file. This is how you change an existing file: write_file would make you re-emit the whole thing. `old` must appear exactly once unless `all` is set.",
			Schema: json.RawMessage(`{"type":"object","properties":{
				"path":{"type":"string"},
				"old":{"type":"string","description":"snippet to replace, copied exactly including indentation"},
				"new":{"type":"string","description":"replacement; empty string deletes the snippet"},
				"all":{"type":"boolean","description":"replace every occurrence"}},
				"required":["path","old","new"]}`),
		},
		{
			Name:        "write_file",
			Description: "Create a file or overwrite it completely. Parent directories are created. To change part of an existing file use edit_file instead.",
			Schema: json.RawMessage(`{"type":"object","properties":{
				"path":{"type":"string"},"content":{"type":"string"}},
				"required":["path","content"]}`),
		},
		{
			Name:        "list_dir",
			Description: "List the entries of a directory (name, size, and whether it is a directory).",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		},
	}
	if s.AllowBash {
		defs = append(defs, provider.ToolDef{
			Name:        "bash",
			Description: "Run a shell command in the working directory. Use it for search (grep/find), git, builds and tests.",
			Schema: json.RawMessage(`{"type":"object","properties":{
				"cmd":{"type":"string"},
				"cwd":{"type":"string","description":"directory to run in; defaults to the working directory"},
				"timeout":{"type":"number","description":"seconds, default 60, max 900"}},
				"required":["cmd"]}`),
		})
	}
	return defs
}

// Handles reports whether name belongs to this set.
func (s *Set) Handles(name string) bool {
	switch name {
	case "read_file", "edit_file", "write_file", "list_dir":
		return true
	case "bash":
		return s.AllowBash
	}
	return false
}

// Call runs one tool. Tool-level problems come back as (message, true, nil)
// so the model can read them and retry; only wiring bugs return an error.
func (s *Set) Call(ctx context.Context, name string, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Path    string  `json:"path"`
		Content string  `json:"content"`
		Old     string  `json:"old"`
		New     string  `json:"new"`
		All     bool    `json:"all"`
		Offset  float64 `json:"offset"`
		Limit   float64 `json:"limit"`
		Cmd     string  `json:"cmd"`
		Cwd     string  `json:"cwd"`
		Timeout float64 `json:"timeout"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err), true, nil
		}
	}
	switch name {
	case "read_file":
		return s.read(a.Path, int(a.Offset), int(a.Limit))
	case "edit_file":
		return s.edit(a.Path, a.Old, a.New, a.All)
	case "write_file":
		return s.write(a.Path, a.Content)
	case "list_dir":
		return s.list(a.Path)
	case "bash":
		if !s.AllowBash {
			return "bash is disabled; start anyrun with --allow-bash to enable it", true, nil
		}
		return s.bash(ctx, a.Cmd, a.Cwd, int(a.Timeout))
	}
	return fmt.Sprintf("unknown tool %q", name), true, nil
}

// Resolve turns a tool-supplied path into an absolute one.
func (s *Set) Resolve(p string) string {
	if p == "" {
		return s.Root
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(s.Root, p)
}

func (s *Set) read(path string, offset, limit int) (string, bool, error) {
	if path == "" {
		return "read_file: `path` is required", true, nil
	}
	b, err := os.ReadFile(s.Resolve(path))
	if err != nil {
		return fmt.Sprintf("read_file: %v", err), true, nil
	}
	if offset > 0 || limit > 0 {
		lines := strings.Split(string(b), "\n")
		from := offset - 1
		if from < 0 {
			from = 0
		}
		if from > len(lines) {
			return fmt.Sprintf("read_file: offset %d is past the end of the file (%d lines)", offset, len(lines)), true, nil
		}
		to := len(lines)
		if limit > 0 && from+limit < to {
			to = from + limit
		}
		return strings.Join(lines[from:to], "\n"), false, nil
	}
	if len(b) > maxRead {
		return fmt.Sprintf("%s\n…[truncated at %d bytes of %d — read the rest with offset/limit]", b[:maxRead], maxRead, len(b)), false, nil
	}
	return string(b), false, nil
}

// ApplyEdit replaces oldText inside content. It refuses a snippet that is
// missing or ambiguous rather than picking an occurrence for the model.
func ApplyEdit(content, oldText, newText string, all bool) (string, error) {
	if oldText == "" {
		return "", fmt.Errorf("`old` is required (the snippet to replace)")
	}
	if oldText == newText {
		return "", fmt.Errorf("`old` and `new` are identical — nothing would change")
	}
	n := strings.Count(content, oldText)
	switch {
	case n == 0:
		return "", fmt.Errorf("`old` does not appear in the file — read it again and copy the snippet exactly, including indentation")
	case n > 1 && !all:
		return "", fmt.Errorf("`old` appears %d times — extend the snippet until it is unique, or set `all` to replace every occurrence", n)
	case all:
		return strings.ReplaceAll(content, oldText, newText), nil
	}
	return strings.Replace(content, oldText, newText, 1), nil
}

func (s *Set) edit(path, oldText, newText string, all bool) (string, bool, error) {
	if path == "" {
		return "edit_file: `path` is required", true, nil
	}
	abs := s.Resolve(path)
	b, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Sprintf("edit_file: %v", err), true, nil
	}
	out, err := ApplyEdit(string(b), oldText, newText, all)
	if err != nil {
		return fmt.Sprintf("edit_file: %v", err), true, nil
	}
	info, err := os.Stat(abs)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(abs, []byte(out), mode); err != nil {
		return fmt.Sprintf("edit_file: %v", err), true, nil
	}
	return fmt.Sprintf("edited %s (%d bytes)", path, len(out)), false, nil
}

func (s *Set) write(path, content string) (string, bool, error) {
	if path == "" {
		return "write_file: `path` is required", true, nil
	}
	abs := s.Resolve(path)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Sprintf("write_file: %v", err), true, nil
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("write_file: %v", err), true, nil
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), false, nil
}

func (s *Set) list(path string) (string, bool, error) {
	entries, err := os.ReadDir(s.Resolve(path))
	if err != nil {
		return fmt.Sprintf("list_dir: %v", err), true, nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name()+"/")
			continue
		}
		size := int64(-1)
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		names = append(names, fmt.Sprintf("%s\t%d", e.Name(), size))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(empty directory)", false, nil
	}
	return strings.Join(names, "\n"), false, nil
}

func (s *Set) bash(ctx context.Context, cmd, cwd string, timeout int) (string, bool, error) {
	if strings.TrimSpace(cmd) == "" {
		return "bash: `cmd` is required", true, nil
	}
	if timeout <= 0 {
		timeout = 60
	}
	if timeout > 900 {
		timeout = 900
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	c.Dir = s.Root
	if cwd != "" {
		c.Dir = s.Resolve(cwd)
	}
	out, err := c.CombinedOutput()
	text := string(out)
	if len(text) > maxOutput {
		text = text[:maxOutput] + fmt.Sprintf("\n…[truncated at %d bytes]", maxOutput)
	}
	code := c.ProcessState.ExitCode()
	if ctx.Err() != nil {
		return fmt.Sprintf("%s\n[timed out after %ds]", text, timeout), true, nil
	}
	if err != nil && code < 0 {
		return fmt.Sprintf("bash: %v\n%s", err, text), true, nil
	}
	if text == "" {
		text = "(no output)"
	}
	return fmt.Sprintf("%s\n[exit %d]", text, code), false, nil
}
