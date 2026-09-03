// Package mcpclient spawns the MCP servers from --mcp-config and bridges
// their tools to the model. Tool names use the Claude convention
// mcp__<server>__<tool> so claude-agent's event log and tool patterns
// keep working unchanged.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/pandeptwidyaop/anyrun/internal/provider"
)

// rpc is the slice of client.Client the pool uses; a seam for in-process
// test servers.
type rpc interface {
	ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error)
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
	Close() error
}

type route struct{ server, tool string }

type Pool struct {
	clients map[string]rpc
	defs    []provider.ToolDef
	routes  map[string]route
}

// Start connects every server in cfg (stdio spawn or http/sse dial),
// initializes it, and lists tools. Individual server failures are soft —
// warn on stderr and continue with the rest, same as the Claude CLI — but
// if EVERY configured server fails, that is fatal: an agent without any of
// its tools must not silently answer toolless.
func Start(ctx context.Context, cfg Config) (*Pool, error) {
	clients := make(map[string]rpc, len(cfg.MCPServers))
	for name, sc := range cfg.MCPServers {
		c, err := connect(ctx, sc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "anyrun: MCP server %q unavailable, skipping: %v\n", name, err)
			continue
		}
		clients[name] = c
	}
	if len(clients) == 0 && len(cfg.MCPServers) > 0 {
		return nil, fmt.Errorf("mcpclient: all %d MCP servers failed to start", len(cfg.MCPServers))
	}
	p, err := startWithClients(ctx, clients)
	if err != nil {
		closeAll(clients)
		return nil, err
	}
	return p, nil
}

func connect(ctx context.Context, sc ServerConfig) (*client.Client, error) {
	switch {
	case sc.URL != "" && sc.Type == "sse":
		c, err := client.NewSSEMCPClient(sc.URL, client.WithHeaders(sc.Headers))
		if err != nil {
			return nil, err
		}
		return c, c.Start(ctx)
	case sc.URL != "": // "http", "streamable-http", or unspecified with a URL
		c, err := client.NewStreamableHttpClient(sc.URL, transport.WithHTTPHeaders(sc.Headers))
		if err != nil {
			return nil, err
		}
		return c, c.Start(ctx)
	case sc.Command != "":
		env := os.Environ()
		for k, v := range sc.Env {
			env = append(env, k+"="+v)
		}
		return client.NewStdioMCPClient(sc.Command, env, sc.Args...) // auto-starts
	default:
		return nil, fmt.Errorf("neither command nor url configured")
	}
}

func startWithClients(ctx context.Context, clients map[string]rpc) (*Pool, error) {
	p := &Pool{clients: clients, routes: map[string]route{}}
	for name, c := range clients {
		if cc, ok := c.(*client.Client); ok && !cc.IsInitialized() {
			if _, err := cc.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
				fmt.Fprintf(os.Stderr, "anyrun: MCP server %q failed to initialize, skipping: %v\n", name, err)
				_ = c.Close()
				delete(p.clients, name)
				continue
			}
		}
		list, err := c.ListTools(ctx, mcp.ListToolsRequest{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "anyrun: MCP server %q failed to list tools, skipping: %v\n", name, err)
			_ = c.Close()
			delete(p.clients, name)
			continue
		}
		for _, t := range list.Tools {
			full := "mcp__" + name + "__" + t.Name
			schema, err := json.Marshal(t.InputSchema)
			if err != nil || len(schema) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			p.defs = append(p.defs, provider.ToolDef{Name: full, Description: t.Description, Schema: schema})
			p.routes[full] = route{server: name, tool: t.Name}
		}
	}
	return p, nil
}

func (p *Pool) Tools() []provider.ToolDef { return p.defs }

// Call executes a prefixed tool. Tool-level failures come back as
// (content, true, nil) so the model can see and react to them; only
// unrecoverable plumbing problems use the error return.
func (p *Pool) Call(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	r, ok := p.routes[name]
	if !ok {
		return fmt.Sprintf("unknown tool %q", name), true, nil
	}
	var argMap map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argMap); err != nil {
			return fmt.Sprintf("invalid tool arguments: %v", err), true, nil
		}
	}
	res, err := p.clients[r.server].CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: r.tool, Arguments: argMap},
	})
	if err != nil {
		return fmt.Sprintf("tool call failed: %v", err), true, nil
	}
	var parts []string
	for _, c := range res.Content {
		if tc, ok := mcp.AsTextContent(c); ok {
			parts = append(parts, tc.Text)
		} else {
			parts = append(parts, "[non-text content]")
		}
	}
	return strings.Join(parts, "\n"), res.IsError, nil
}

func (p *Pool) Close() { closeAll(p.clients) }

func closeAll(clients map[string]rpc) {
	for _, c := range clients {
		_ = c.Close()
	}
}
