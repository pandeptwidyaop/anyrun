package mcpclient

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestLoadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mcp.json")
	os.WriteFile(p, []byte(`{"mcpServers":{"agent":{"command":"/bin/agent","args":["mcp"],"env":{"A":"1"}}}}`), 0o600)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	s, ok := cfg.MCPServers["agent"]
	if !ok || s.Command != "/bin/agent" || len(s.Args) != 1 || s.Env["A"] != "1" {
		t.Errorf("bad config: %+v", cfg)
	}
}

func echoServer(t *testing.T) *client.Client {
	t.Helper()
	srv := server.NewMCPServer("fixture", "1.0")
	srv.AddTool(
		mcp.NewTool("echo", mcp.WithDescription("echo back"), mcp.WithString("msg")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args, _ := req.Params.Arguments.(map[string]any)
			msg, _ := args["msg"].(string)
			return mcp.NewToolResultText("echo: " + msg), nil
		})
	srv.AddTool(
		mcp.NewTool("boom", mcp.WithDescription("always errors")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("kaboom"), nil
		})
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatalf("in-process client: %v", err)
	}
	return c
}

func testPool(t *testing.T) *Pool {
	t.Helper()
	p, err := startWithClients(context.Background(), map[string]rpc{"agent": echoServer(t)})
	if err != nil {
		t.Fatalf("startWithClients: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestToolsArePrefixedWithSchemas(t *testing.T) {
	p := testPool(t)
	defs := p.Tools()
	if len(defs) != 2 {
		t.Fatalf("tools = %d, want 2", len(defs))
	}
	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
		if len(d.Schema) == 0 || !strings.Contains(string(d.Schema), "object") {
			t.Errorf("tool %s: missing schema: %s", d.Name, d.Schema)
		}
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "mcp__agent__echo") || !strings.Contains(joined, "mcp__agent__boom") {
		t.Errorf("bad names: %v", names)
	}
}

func TestCallRoundTrip(t *testing.T) {
	p := testPool(t)
	out, isErr, err := p.Call(context.Background(), "mcp__agent__echo", json.RawMessage(`{"msg":"halo"}`))
	if err != nil || isErr {
		t.Fatalf("Call: err=%v isErr=%v", err, isErr)
	}
	if out != "echo: halo" {
		t.Errorf("out = %q", out)
	}
}

func TestCallErrorsAreSoft(t *testing.T) {
	p := testPool(t)
	out, isErr, err := p.Call(context.Background(), "mcp__agent__boom", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("transport err: %v", err)
	}
	if !isErr || !strings.Contains(out, "kaboom") {
		t.Errorf("want soft error, got out=%q isErr=%v", out, isErr)
	}

	out, isErr, err = p.Call(context.Background(), "mcp__nope__missing", json.RawMessage(`{}`))
	if err != nil || !isErr || !strings.Contains(out, "unknown tool") {
		t.Errorf("unknown tool: out=%q isErr=%v err=%v", out, isErr, err)
	}
}
