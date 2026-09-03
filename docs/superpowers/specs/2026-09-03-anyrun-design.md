# anyrun — claude-CLI-compatible runner for non-Claude models (Design)

Date: 2026-09-03
Status: approved by Kak Pande (Telegram, 3 Sep 2026)

## Problem

claude-agent's only runner spawns `claude --print`. The Claude CLI caps
context by model identity: 1M context is only honored for Anthropic models
on api.anthropic.com. Any other provider or model — even ones with native
1M+ windows (Gemini, GPT-4.1-class, long-context Qwen) — cannot use their
full context through the CLI.

## Goal

A standalone Go binary (`anyrun`, working name) that is a **drop-in
replacement for the subset of `claude --print` behavior that claude-agent
actually uses**, speaking three provider API styles: Anthropic, OpenAI
(chat completions), Gemini (generateContent). Sarah (the daily-driver
agent) must be able to run on a non-Claude model with full native context,
full MCP tools, streaming, sessions, and personality injection.

Explicit non-goal: bug-for-bug compatibility with the full Claude CLI. The
only consumer is `internal/claude/runner.go` + its stream-json parser (and
forks like btw-claude-agent). Compatibility is defined by that subset.

## Decisions (from brainstorming)

- Daily-driver use case → MCP, streaming, resume, media are all required.
- Standalone binary in its own repo (option a), not a package inside
  claude-agent, not a Pi RPC wrapper. Reusable by btw-claude-agent.
- Three API styles: `anthropic`, `openai`, `gemini`. OpenAI style covers
  OpenRouter/vLLM/DeepSeek etc.
- Provider config via env (fits the `models` collection's per-model `env`
  JSON): `ANYRUN_API_STYLE`, `ANYRUN_BASE_URL`, `ANYRUN_API_KEY`.
- claude-agent needs exactly one tweak: if a model's env carries
  `RUNNER_BIN=anyrun`, `runner.go` spawns that binary instead of `claude`.
  Everything else (parser, worker, personality, MCP config generation) is
  untouched.

## CLI surface (the compatibility contract)

Flags accepted, matching claude-agent's `buildArgs()`:

```
--print                        required no-op (we are always non-interactive)
--output-format stream-json    only supported value
--input-format stream-json     only supported value
--verbose                      accepted; controls stderr debug logging
--session-id <uuid>            create session
--resume <uuid>                continue session
--append-system-prompt-file <path>
--mcp-config <path>            Claude-CLI-format MCP config JSON
--model <id>                   provider model id
--max-turns <n>                agentic loop cap
--dangerously-skip-permissions accepted and ignored
```

Stdin: single JSON envelope per invocation, Claude CLI shape:
`{"type":"user","message":{"role":"user","content":[<blocks>]}}` where
blocks are text and optionally Anthropic-style `image`/`document` base64
blocks.

Stdout: stream-json events — the subset claude-agent's parser consumes
(`system/init`, `assistant` message events with `content_block_delta`,
`tool_use` blocks, and the final `result` event carrying usage). Event
shapes verified against the real CLI's output; the leaked reverse repo
(github.com/tanbiralam/claude-code) serves as a research reference for
event/session semantics, not as code to copy.

Exit codes: 0 on success; non-zero on fatal error, with a `result` event
carrying `is_error: true` (same as CLI behavior the worker already
handles).

## Architecture

```
cmd/anyrun/          flag parsing, stdin envelope decode, wiring
internal/session/    JSONL session store  (~/.anyrun/sessions/<uuid>.jsonl)
internal/loop/       agentic loop: messages → provider → events → tools → repeat
internal/provider/   Provider interface + anthropic/openai/gemini adapters
internal/mcpclient/  MCP config parsing, stdio client pool (mcp-go), tool bridge
internal/emit/       stream-json event writer (stdout)
```

### Session store

One JSONL file per session id: full message history (user, assistant,
tool_use, tool_result), appended after each turn. `--resume` loads and
continues. Every provider call replays the entire history — this is where
the full native context window gets used; anyrun never truncates or
compacts. If history exceeds the model's window, the provider's error is
surfaced as-is (fatal result event); compaction is claude-agent's concern,
not anyrun's.

### Agentic loop (per invocation)

1. Assemble: system prompt (from `--append-system-prompt-file`) + session
   history + new user message from stdin envelope.
2. Call provider adapter, streaming.
3. Re-emit as claude-CLI stream-json events on stdout.
4. If the model emitted tool calls: execute each via the MCP bridge,
   append tool results to history, go to 2.
5. On completion or `--max-turns`: emit `result` event (with usage
   tokens), persist session, exit.

### Provider adapters

Single interface, three implementations:

```go
type Provider interface {
    // ChatStream sends the full message list + tool definitions and
    // streams back deltas, tool calls, and final usage.
    ChatStream(ctx context.Context, req ChatRequest) (<-chan Event, error)
}
```

All format translation (messages, tool schemas, media blocks, usage
accounting) lives inside the adapter. The loop is provider-agnostic.

- `anthropic` — near passthrough (`/v1/messages`, SSE).
- `openai` — chat completions (`/v1/chat/completions`, SSE, `tools` /
  `tool_calls`). Covers OpenRouter, vLLM, DeepSeek, etc.
- `gemini` — `generateContent` streaming, `functionDeclarations`.

### MCP bridge

- Parse `--mcp-config` (Claude CLI format, already generated by
  claude-agent — reused verbatim).
- Spawn each stdio server (`./agent mcp`), via `mcp-go`.
- `tools/list` at startup → translate JSON Schemas to each provider's tool
  format (adapter's job).
- Model tool call → `tools/call` → result appended to history as
  tool_result. Tool execution errors become error-flagged tool results
  (the model sees them and can react), not fatal crashes.

### Media

Inline Anthropic-style `image`/`document` blocks arrive in the stdin
envelope (claude-agent already builds these for models with
`INLINE_MEDIA_CONTENT_BLOCKS=true`). Adapters translate: OpenAI →
`image_url` data URI; Gemini → `inline_data`. The text-only fallback path
(`[Attached file: …]` + `read_media` MCP tool) needs nothing from anyrun.

## Accepted trade-offs

- **Cost:** replaying full history each turn is expensive on providers
  without prompt caching. Anthropic and Gemini cache; OpenAI-style depends
  on the model/gateway. Conscious trade-off for full-context fidelity.
- **No compaction in anyrun:** context overflow errors surface to the
  worker; handling stays upstream.
- Working name `anyrun` may change; repo lives at
  `/home/devops/projects/anyrun` (local, no remote yet — same as vidbox).

## Testing

- Unit: fake provider servers (`httptest`) per adapter; golden-file
  comparison of emitted stream-json against captures from the real Claude
  CLI (record `claude --print --output-format stream-json` output for a
  text-only turn and a tool-use turn, and match the consumed subset).
- MCP bridge tested against a tiny in-process stdio MCP server fixture.
- Integration: a cheap OpenRouter model end-to-end (session create →
  tool call → resume), gated behind an env var like the vidbox B2 tests.
- Final acceptance: register a model in claude-agent's `models` collection
  with `RUNNER_BIN=anyrun`, chat via web UI, verify memory/knowledge MCP
  tools work and usage/cost lands in `chat_history`.

## Rollout

1. anyrun MVP: openai style first (OpenRouter unlocks the most models),
   then anthropic, then gemini.
2. `runner.go` tweak in claude-agent (`RUNNER_BIN` env, default `claude`).
3. Side-by-side trial: Sarah on Claude as primary; a test agent on a 1M
   non-Claude model until quality is proven.
