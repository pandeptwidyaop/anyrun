# anyrun

A Claude-Code-compatible agent runner for models that are not Claude.

anyrun speaks the same command-line contract as the Claude Code CLI in print
mode — the same flags, the same stream-json on stdin and stdout — but it talks
to any OpenAI-style chat API. Point it at OpenRouter, a local llama.cpp server,
vLLM, or anything else that serves `POST /chat/completions`, and every tool
built around the Claude CLI keeps working unchanged.

```
                stream-json (stdin)                  POST /chat/completions
   your host ───────────────────────▶  anyrun  ─────────────────────────────▶  any model
             ◀───────────────────────         ◀─────────────────────────────
                stream-json (stdout)                   tool calls / text
```

## Install

Go 1.21 or newer:

```bash
go build -o ~/.local/bin/anyrun ./cmd/anyrun
```

## Quick start

anyrun reads one user message from stdin and writes the turn as stream-json to
stdout. A session id is required — it is the key under which the conversation
is stored and resumed.

```bash
export ANYRUN_BASE_URL=https://openrouter.ai/api/v1
export ANYRUN_API_KEY=sk-or-v1-…

printf '{"type":"user","message":{"role":"user","content":[{"type":"text","text":"list the go files and summarise the loop package"}]}}' \
  | anyrun --print --output-format stream-json --input-format stream-json --verbose \
           --model minimax/minimax-m3:free --max-turns 30 --session-id my-session
```

Continue the same conversation with `--resume` instead of `--session-id`:

```bash
printf '{"type":"user","message":{"role":"user","content":[{"type":"text","text":"now add a test for it"}]}}' \
  | anyrun --print --output-format stream-json --input-format stream-json \
           --model minimax/minimax-m3:free --resume my-session
```

## Tools

anyrun ships its own file tools, so it is useful on a codebase with no MCP
server in front of it. Tools from MCP servers are added alongside them.

| Tool | What it does |
| --- | --- |
| `read_file` | Reads a text file; `offset`/`limit` (1-based lines) page through files too large to read at once. |
| `edit_file` | Replaces an exact snippet. Refuses a snippet that is missing or appears more than once (unless `all` is set) rather than guessing which one you meant. |
| `write_file` | Creates a file or overwrites it completely; parent directories are created. |
| `list_dir` | Lists a directory: names, sizes, and which entries are directories. |
| `bash` | Runs a shell command. **Opt-in** — see below. |

`edit_file` exists because rewriting a whole file to change one line is how
small models corrupt files: the rewrite runs past their output budget and the
tail is silently lost. Editing a snippet costs a few tokens regardless of file
size, and a wrong snippet fails loudly instead of half-applying.

Relative paths resolve against the working directory anyrun was started in;
absolute paths and `~/…` are used as given.

### Why bash is opt-in

`bash` only appears with `--allow-bash` (or `ANYRUN_ALLOW_BASH=1`). A host that
brokers shell access itself usually screens the commands before running them,
and needs to be sure anyrun does not open a second, unscreened path. Where that
is not a concern — anyrun run straight from a terminal, say — turn it on.

## Flags

| Flag | Meaning |
| --- | --- |
| `--model` | Model id passed straight to the API (required). |
| `--session-id` / `--resume` | Conversation key; one of the two is required. |
| `--system-prompt-file` | File whose contents become the system prompt. |
| `--max-turns` | Stop after this many model turns; `0` = unlimited. |
| `--mcp-config` | JSON file describing MCP servers to connect. |
| `--disallowedTools` | Comma-separated tool names or `prefix*` globs to hide from the model. |
| `--allow-bash` | Enable the built-in `bash` tool. |
| `--verbose` | Extra diagnostics on stderr. |
| `--print`, `--output-format`, `--input-format` | Claude CLI compatibility; only `stream-json` in and out is supported. |
| `--allowedTools`, `--dangerously-skip-permissions` | Accepted and ignored: anyrun does not gate tools behind approval. |

## Environment

| Variable | Meaning |
| --- | --- |
| `ANYRUN_BASE_URL` | API base, e.g. `https://openrouter.ai/api/v1`. Required. |
| `ANYRUN_API_KEY` | Bearer token for that API. Required. |
| `ANYRUN_API_STYLE` | `openai` (default). Other styles are not implemented yet. |
| `ANYRUN_SESSION_DIR` | Where sessions live; defaults to `~/.anyrun/sessions`. |
| `ANYRUN_ALLOW_BASH` | Any non-empty value enables the `bash` tool. |
| `ANYRUN_CONTEXT_WINDOW` / `CLAUDE_CODE_MAX_CONTEXT_TOKENS` | Context size used to decide when to compact. |
| `ANYRUN_COMPACT_AT` / `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` | Fraction (0–1) or percent (1–100) of the window that triggers compaction; default `0.8`. |
| `DISABLE_AUTO_COMPACT` | Any non-empty value turns compaction off. |
| `ANYRUN_TIME_BUDGET_MS` | Wall clock this run has before its caller kills it, in milliseconds. Set by the caller that owns the deadline — see [Sessions and compaction](#sessions-and-compaction). |

## Sessions and compaction

`--resume` continues where the last run stopped. Sessions are written **per
step, not per turn**: the assistant message is appended as soon as it arrives
(before its tools execute) and the tool results as soon as they return. A run
killed mid-flight therefore leaves behind every step it completed.

This matters because the kill is not cooperative. A host that enforces a
timeout SIGKILLs the process group, which cannot be caught, so nothing runs at
exit and whatever was not written yet is gone. Per-step writes bound that loss
to one step instead of the whole turn.

Three consequences worth knowing:

- **The file can end mid-step**, with a `tool_use` whose results never arrived.
  That shape is rejected by OpenAI-compatible providers, so `--resume` closes it
  by appending a `tool_result` saying the run was interrupted and the outcome is
  unknown. The pair is written once and stays closed.
- **A resumed run is told it is resuming.** When the history shows unfinished
  work, a note precedes the new message: the completed steps above are real, the
  request may be a redelivery, continue rather than start over. Without it a
  retried job repeats work that already happened — harmless for a read, not for
  a push.
- **A run near its budget is told to conclude.** Within two turns of
  `--max-turns`, or within `ANYRUN_TIME_BUDGET_MS` minus a 90-second reserve,
  anyrun injects a note asking for a closing report and withdraws the tools so
  the model answers rather than starting another round. The result carries
  `stop_reason` `max_turns` or `time_budget`, and the answer says what is done
  and what is left.

None of the injected notes are persisted — they are guidance for one run, so
they cannot pile up in the file.

When the conversation approaches the configured context window, the oldest
quarter is summarised by the model and replaced by that summary; if summarising
fails the turn carries on uncompacted rather than losing the conversation. The
session file is copied to `<id>.jsonl.pre-compact` first, and the new content
swapped in with a single `rename` — no step removes the live file, so a kill
during compaction cannot lose the history.

## MCP servers

`--mcp-config` takes the familiar `mcpServers` shape. Both stdio and remote
(http/sse) servers work, and tools arrive named `mcp__<server>__<tool>`.

```json
{
  "mcpServers": {
    "workspace": {
      "command": "node",
      "args": ["/opt/mcp/workspace-server.mjs"],
      "env": { "WORKSPACE_ROOT": "/srv/project", "WORKSPACE_TOKEN": "…" }
    },
    "docs": {
      "type": "http",
      "url": "https://mcp.example/v1",
      "headers": { "Authorization": "Bearer …" }
    }
  }
}
```

A failure to start a configured MCP server is fatal: an agent that quietly
answers without the tools it was promised is worse than one that stops.

## Driving anyrun from another program

anyrun is built to be launched per turn by a host program rather than typed at
by a person, and the contract is small enough to wire up in an afternoon:

- pass your standing instructions with `--system-prompt-file`, so they stay out
  of the conversation and out of the token budget of every later turn;
- write the user's turn to stdin as one stream-json envelope;
- give the agent its tools with `--mcp-config`, and keep whatever your host
  already governs (shell access, approvals, remote hosts) on that side;
- map `--session-id` to whatever you call a conversation, then use `--resume`
  with the same id for every following turn;
- read stdout line by line: each line is one event, and the `result` line ends
  the turn.

Because the flags and the stream match the Claude Code CLI in print mode, a
host already speaking to that CLI usually needs to change the command it spawns
and nothing else.

## Protocol

Input — one JSON object on stdin:

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]}}
```

Output — one JSON object per line on stdout: a `system`/`init` line, then
`assistant` messages (text and `tool_use` blocks), `user` messages carrying
`tool_result` blocks, and a final `result` line with `stop_reason`,
`duration_ms`, `total_cost_usd` and token usage. Nothing else is written to
stdout; diagnostics go to stderr.

## Development

```bash
go test ./...
gofmt -l .
```
