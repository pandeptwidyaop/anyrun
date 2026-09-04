package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyEdit(t *testing.T) {
	const src = "one\ntwo\nthree\ntwo\n"
	cases := []struct {
		name    string
		old     string
		new     string
		all     bool
		want    string
		wantErr string
	}{
		{name: "unique snippet", old: "three", new: "THREE", want: "one\ntwo\nTHREE\ntwo\n"},
		{name: "empty new deletes", old: "three\n", new: "", want: "one\ntwo\ntwo\n"},
		{name: "all occurrences", old: "two", new: "TWO", all: true, want: "one\nTWO\nthree\nTWO\n"},
		{name: "missing snippet", old: "four", new: "x", wantErr: "does not appear"},
		{name: "ambiguous snippet", old: "two", new: "TWO", wantErr: "appears 2 times"},
		{name: "no-op edit", old: "three", new: "three", wantErr: "identical"},
		{name: "empty old", old: "", new: "x", wantErr: "`old` is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ApplyEdit(src, c.old, c.new, c.all)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyEdit: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A failed edit must leave the file untouched — a half-applied change is
// worse than none, because the model believes the file already moved on.
func TestEditLeavesFileUntouchedOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Set{Root: dir}
	msg, isErr, err := s.Call(context.Background(), "edit_file", json.RawMessage(`{"path":"a.txt","old":"nope","new":"x"}`))
	if err != nil || !isErr {
		t.Fatalf("want tool-level error, got err=%v isErr=%v", err, isErr)
	}
	if !strings.Contains(msg, "does not appear") {
		t.Errorf("message = %q, want it to explain the snippet was not found", msg)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "hello\n" {
		t.Errorf("file changed to %q", b)
	}
}

func TestEditKeepsFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(path, []byte("echo old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Set{Root: dir}
	if _, isErr, _ := s.Call(context.Background(), "edit_file", json.RawMessage(`{"path":"run.sh","old":"old","new":"new"}`)); isErr {
		t.Fatal("edit reported an error")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 — an edited script must stay executable", info.Mode().Perm())
	}
}

func TestReadWindow(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("l1\nl2\nl3\nl4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Set{Root: dir}
	got, isErr, _ := s.Call(context.Background(), "read_file", json.RawMessage(`{"path":"f.txt","offset":2,"limit":2}`))
	if isErr {
		t.Fatalf("read failed: %s", got)
	}
	if got != "l2\nl3" {
		t.Errorf("got %q, want \"l2\\nl3\"", got)
	}
	if msg, isErr, _ := s.Call(context.Background(), "read_file", json.RawMessage(`{"path":"f.txt","offset":99}`)); !isErr || !strings.Contains(msg, "past the end") {
		t.Errorf("offset past EOF should explain itself, got %q (isErr=%v)", msg, isErr)
	}
}

func TestWriteCreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	s := &Set{Root: dir}
	if msg, isErr, _ := s.Call(context.Background(), "write_file", json.RawMessage(`{"path":"deep/nested/f.txt","content":"hi"}`)); isErr {
		t.Fatalf("write failed: %s", msg)
	}
	b, err := os.ReadFile(filepath.Join(dir, "deep/nested/f.txt"))
	if err != nil || string(b) != "hi" {
		t.Fatalf("file = %q, err = %v", b, err)
	}
}

func TestResolve(t *testing.T) {
	s := &Set{Root: "/work"}
	if got := s.Resolve("src/a.go"); got != "/work/src/a.go" {
		t.Errorf("relative path = %q", got)
	}
	if got := s.Resolve("/etc/hosts"); got != "/etc/hosts" {
		t.Errorf("absolute path = %q", got)
	}
	if got := s.Resolve(""); got != "/work" {
		t.Errorf("empty path = %q", got)
	}
}

// bash stays off unless asked for: a host that brokers shell access itself
// must not find a second, unchecked path through anyrun.
func TestBashIsOptIn(t *testing.T) {
	s := &Set{Root: t.TempDir()}
	if s.Handles("bash") {
		t.Error("bash should not be handled while disabled")
	}
	for _, d := range s.Defs() {
		if d.Name == "bash" {
			t.Error("bash should not be advertised while disabled")
		}
	}
	msg, isErr, _ := s.Call(context.Background(), "bash", json.RawMessage(`{"cmd":"echo hi"}`))
	if !isErr || !strings.Contains(msg, "--allow-bash") {
		t.Errorf("disabled bash should say how to enable it, got %q", msg)
	}
}

func TestBashRunsInRoot(t *testing.T) {
	dir := t.TempDir()
	s := &Set{Root: dir, AllowBash: true}
	got, isErr, err := s.Call(context.Background(), "bash", json.RawMessage(`{"cmd":"pwd"}`))
	if err != nil || isErr {
		t.Fatalf("bash failed: %v %q", err, got)
	}
	// macOS reports /private/var… for /var…; comparing the base name is enough.
	if !strings.Contains(got, filepath.Base(dir)) {
		t.Errorf("pwd = %q, want it inside %q", got, dir)
	}
	if !strings.Contains(got, "[exit 0]") {
		t.Errorf("output should carry the exit code, got %q", got)
	}
}

func TestBashReportsExitCode(t *testing.T) {
	s := &Set{Root: t.TempDir(), AllowBash: true}
	got, _, _ := s.Call(context.Background(), "bash", json.RawMessage(`{"cmd":"exit 3"}`))
	if !strings.Contains(got, "[exit 3]") {
		t.Errorf("got %q, want it to report exit 3", got)
	}
}

func TestDefsHaveUsableSchemas(t *testing.T) {
	s := &Set{Root: ".", AllowBash: true}
	defs := s.Defs()
	if len(defs) != 5 {
		t.Fatalf("got %d tools, want 5", len(defs))
	}
	for _, d := range defs {
		var schema struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		if err := json.Unmarshal(d.Schema, &schema); err != nil {
			t.Errorf("%s: schema is not valid JSON: %v", d.Name, err)
			continue
		}
		if schema.Type != "object" || len(schema.Properties) == 0 || len(schema.Required) == 0 {
			t.Errorf("%s: schema must be an object with properties and required fields", d.Name)
		}
		if d.Description == "" {
			t.Errorf("%s: description is empty — the model picks tools by it", d.Name)
		}
	}
}
