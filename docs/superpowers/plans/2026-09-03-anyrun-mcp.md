# anyrun Milestone 2: MCP bridge + agentic tool loop

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** anyrun spawns the MCP servers from `--mcp-config`, exposes their tools to the model, executes tool calls in an agentic loop, and emits `tool_use` / `tool_result` events — the milestone that lets a claude-agent persona actually live on a non-Claude model.

**Architecture:** New `internal/mcpclient` package (mcp-go v1.0.0, stdio transport) owns server processes and tool routing; tool names use the Claude convention `mcp__<server>__<tool>` so claude-agent's event log and tool patterns keep working. `provider.Request` gains `Tools`, `provider.Result` gains `ToolCalls`. The loop becomes multi-turn: model → tools → model, capped by `--max-turns`, usage summed across calls. History stores Anthropic-shaped `tool_use`/`tool_result` blocks; the openai adapter translates both directions.

**Tech Stack:** Go, `github.com/mark3labs/mcp-go v1.0.0` (verified API: `NewStdioMCPClient(command, env, args...)`, `Initialize`, `ListTools`, `CallTool`).

**Verified interop facts:**
- `--mcp-config` JSON: `{"mcpServers":{"<name>":{"command":"...","args":[...],"env":{...}}}}` (from claude-agent `mcpconfig.go`).
- Parser consumes `tool_use` blocks `{type,id,name,input}` (input = raw JSON object) in `assistant` events and `tool_result` blocks `{type,tool_use_id,content}` (content = string) in `user` events.
- `--disallowedTools` is the reliable restriction flag; `--allowedTools` only auto-approves → anyrun honors disallowed (glob `*` suffix match), ignores allowed.

---

### Task 1: mcpclient — config, tool defs, pool

**Files:**
- Create: `internal/mcpclient/config.go`, `internal/mcpclient/pool.go`
- Test: `internal/mcpclient/mcpclient_test.go`

**Interfaces:**

```go
// config.go
type ServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`  // non-stdio servers: skipped with a stderr note
}
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}
func LoadConfig(path string) (Config, error)

// pool.go
type ToolDef struct {
	Name        string          // mcp__<server>__<tool>
	Description string
	Schema      json.RawMessage // JSON Schema from tools/list
}
type Pool struct { /* clients by server name, route map, defs */ }
func Start(ctx context.Context, cfg Config) (*Pool, error)
func (p *Pool) Tools() []ToolDef
func (p *Pool) Call(ctx context.Context, name string, args json.RawMessage) (content string, isErr bool, err error)
func (p *Pool) Close()
```

Implementation notes (bindings verified against `go doc`):
- Spawn: `client.NewStdioMCPClient(cfg.Command, envSlice, cfg.Args...)` where
  `envSlice = os.Environ() + k=v from cfg.Env` (the agent MCP server needs
  `BACKEND_URL`/`APP_TOKEN` from config env).
- `Initialize(ctx, mcp.InitializeRequest{})` then `ListTools(ctx, mcp.ListToolsRequest{})`.
- Def naming: `"mcp__" + serverName + "__" + tool.Name`. Schema: marshal
  `tool.InputSchema` (fall back to `{"type":"object"}` on empty).
- `Call`: split the prefixed name back into (server, tool); unknown name →
  `("unknown tool", true, nil)` so the model can recover. Arguments:
  unmarshal args into `map[string]any`, send via
  `mcp.CallToolRequest{Params: mcp.CallToolParams{Name, Arguments}}`.
- Result → string: concatenate `mcp.TextContent.Text` items (use
  `mcp.AsTextContent`/type switch); non-text content becomes
  `[non-text content]`. `isErr` = `result.IsError`.
- Tool execution failure (transport error) returns the error string as
  content with isErr=true — the model sees it; anyrun does not crash.

Unit tests use `client.NewInProcessClient(server.NewMCPServer(...))` with a
registered `echo` tool — no subprocess needed. Pool internals accept a
pre-built client via an unexported seam (`startWithClients`) so tests skip
`Start`. Test: prefixed naming, schema passthrough, echo round trip, error
mapping, `Close` idempotence.

- [ ] Write tests → red
- [ ] Implement → green (`go test ./internal/mcpclient/`)
- [ ] Commit: `feat: MCP client pool with claude-style tool naming`

---

### Task 2: provider types + emit tool events

**Files:**
- Modify: `internal/provider/provider.go`
- Modify: `internal/emit/emit.go`
- Test: extend `internal/emit/emit_test.go`

```go
// provider.go additions
type ToolDef struct {
	Name, Description string
	Schema            json.RawMessage
}
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}
// Request gains: Tools []ToolDef
// Result gains:  ToolCalls []ToolCall  (StopReason "tool_use" when non-empty)
```

```go
// emit.go additions
// AssistantTurn emits one assistant event: optional text block + tool_use blocks.
func (w *Writer) AssistantTurn(text string, calls []provider.ToolCall) error {
	var content []map[string]any
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, c := range calls {
		content = append(content, map[string]any{
			"type": "tool_use", "id": c.ID, "name": c.Name,
			"input": json.RawMessage(c.Args),
		})
	}
	return w.write(map[string]any{"type": "assistant",
		"message": map[string]any{"role": "assistant", "content": content}})
}

type ToolResult struct {
	ID      string
	Content string
}

// ToolResults emits one user event with tool_result blocks.
func (w *Writer) ToolResults(results []ToolResult) error { ... same pattern ... }
```

`AssistantText(t)` becomes `AssistantTurn(t, nil)` internally (keep the old
method as a thin wrapper so milestone-1 call sites stay valid).

Tests: tool_use block shape (`input` must be a JSON **object**, not a
string), tool_result block shape, text+call combination.

- [ ] Tests red → implement → green
- [ ] Commit: `feat: tool_use/tool_result events and provider tool types`

---

### Task 3: openai adapter — tools both directions

**Files:**
- Modify: `internal/provider/openai/openai.go`
- Test: extend `internal/provider/openai/openai_test.go`

Request side:
- `tools`: `[{"type":"function","function":{"name":d.Name,"description":d.Description,"parameters":<d.Schema>}}]` (omit key when no tools).
- History translation replaces `flattenText` with `toOpenAI(msgs)`:
  - user msg, text blocks only → `{"role":"user","content":<joined text>}`
  - user msg containing `tool_result` blocks → one `{"role":"tool","tool_call_id":<tool_use_id>,"content":<content>}` message **per block** (OpenAI's required shape)
  - assistant msg → `{"role":"assistant","content":<text or "">}` plus
    `tool_calls: [{"id":<id>,"type":"function","function":{"name":<name>,"arguments":<input as STRING>}}]` when tool_use blocks present
  - block decode uses a superset struct: `{type,text,id,name,input(json.RawMessage),tool_use_id,content}`

Response side:
- `choices[0].message.tool_calls[].function.arguments` is a **string** of JSON → store as `ToolCall.Args` (validate: if not valid JSON, wrap as `{}` and log to stderr).
- `finish_reason "tool_calls"` → StopReason `tool_use`.

Tests (fake server, scripted): (1) request carries `tools` and translated
tool history in the right shapes; (2) response with `tool_calls` maps to
`Result.ToolCalls` with parsed args and StopReason tool_use.

- [ ] Tests red → implement → green
- [ ] Commit: `feat: tool calling in the openai adapter`

---

### Task 4: agentic loop

**Files:**
- Modify: `internal/loop/loop.go`
- Test: extend `internal/loop/loop_test.go`

```go
// Deps additions
Tools    []provider.ToolDef
CallTool func(ctx context.Context, name string, args json.RawMessage) (string, bool, error)
MaxTurns int // 0 = unlimited
```

Turn algorithm:

```go
history load → msgs = history + userMsg; newMsgs = [userMsg]
var totalUsage; var finalText string; stop := "end_turn"
for turn := 0; ; turn++ {
    if d.MaxTurns > 0 && turn >= d.MaxTurns { stop = "max_turns"; break }
    res := d.Provider.Chat(ctx, {Model, System, Messages: msgs, Tools: d.Tools})
    totalUsage += res.Usage
    emit.AssistantTurn(res.Text, res.ToolCalls)
    aMsg := assistant msg w/ text + tool_use blocks; msgs, newMsgs append aMsg
    if len(res.ToolCalls) == 0 { finalText = res.Text; stop = res.StopReason; break }
    results := for each call → d.CallTool(...); errors become content, isErr noted in content prefix "ERROR: "
    emit.ToolResults(results)
    rMsg := user msg w/ tool_result blocks; msgs, newMsgs append rMsg
}
Store.Append(sessionID, newMsgs...)   // persist only on full success
emit.Result(finalText, stop, summed usage, ...)
```

Block builders mirror the emit shapes (tool_use `input` raw JSON object;
tool_result `content` string) so the JSONL history replays into the
adapter's `toOpenAI` translator without loss.

Tests with a scripted fake provider (call #1 returns one ToolCall, call #2
returns text) + fake CallTool: (1) event order assistant→user(tool_result)
→assistant→result; (2) persisted history = 4 msgs with correct block types;
(3) MaxTurns=1 stops before the second provider call; (4) usage summed.

- [ ] Tests red → implement → green
- [ ] Commit: `feat: agentic tool loop with max-turns and usage totals`

---

### Task 5: CLI wiring

**Files:**
- Modify: `cmd/anyrun/main.go`
- Test: extend `cmd/anyrun/main_test.go`

- `parseArgs` captures `--disallowedTools` into `cfg.DisallowedTools string`.
- `run()`: when `cfg.MCPConfigFile != ""` → `LoadConfig` + `mcpclient.Start`
  + `defer pool.Close()`; tools = pool.Tools() filtered by
  `matchesAny(cfg.DisallowedTools, name)` (comma-separated globs, `*`
  suffix wildcard only — same shapes buildArgs produces); Deps gains
  `Tools`, `CallTool: pool.Call`, `MaxTurns: cfg.MaxTurns`.
- MCP startup failure → `fail(w, err)` (fatal: an agent without its tools
  must not silently answer toolless).

Tests: disallowed filter (exact + `mcp__agent__*` pattern), parseArgs keeps
capturing the flag.

- [ ] Tests red → implement → green; `go test ./... && go vet ./...`
- [ ] Commit: `feat: wire MCP pool and disallowed-tools filter into the CLI`

---

### Task 6: end-to-end smoke + push

- [ ] Build a throwaway stdio MCP fixture at `/tmp/anyrun-mcptool/` (mcp-go
  `server.NewMCPServer` + one tool `get_time` returning a fixed string +
  `server.ServeStdio`), plus `--mcp-config /tmp/anyrun-mcp.json` pointing
  at it under server name `agent`.
- [ ] Fake-provider smoke: extend the fake OpenAI server to return a
  scripted `tool_calls` response first, then a text response; verify the
  full event sequence and that the session file holds tool blocks.
- [ ] Real smoke: DeepSeek flash (`ANYRUN_BASE_URL=https://api.deepseek.com`,
  model `deepseek-v4-flash`, key from claude-agent models collection) with
  the fixture config; prompt: "pakai tool get_time untuk cek jam sekarang".
  Expected: tool_use event → tool_result event → answer citing the fixed
  string; resume still works.
- [ ] `git push origin main`; update `anyrun-project` knowledge status.

---

## Deliberately out of scope (later milestones)

claude-agent `runner.go` tweak (AgentEnv CLAUDE_BIN), anthropic/gemini
adapters, media blocks, streaming, prompt-cache hints, thinking blocks.
