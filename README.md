# cli-agent-mcp

[![ci](https://github.com/andresh0816/cli-agent-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/andresh0816/cli-agent-mcp/actions/workflows/ci.yml)
[![release](https://github.com/andresh0816/cli-agent-mcp/actions/workflows/release.yml/badge.svg)](https://github.com/andresh0816/cli-agent-mcp/actions/workflows/release.yml)
[![latest release](https://img.shields.io/github/v/release/andresh0816/cli-agent-mcp?sort=semver)](https://github.com/andresh0816/cli-agent-mcp/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/andresh0816/cli-agent-mcp.svg)](https://pkg.go.dev/github.com/andresh0816/cli-agent-mcp)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A single-binary **MCP (Model Context Protocol) stdio server** that lets an MCP
client — such as **Claude Desktop** — drive a **local headless CLI coding agent**
as a background worker, with **live progress streaming**.

Out of the box it drives **Claude Code** and **Cursor**, and any other CLI tool
can be wired up with environment variables alone — no code required.

Instead of copy-pasting between a chat window and a terminal agent, the client
delegates a task, watches the worker narrate its progress in real time, and gets
the result inline — as if it had done the work itself.

## Why run the agent locally?

The worker is spawned as a **child process of this server and inherits its
environment**. That means it can reach whatever the host machine can reach:

- private networks and VPN routes,
- internal hosts via your **SSH agent** (1Password, `ssh-agent`, Pageant, …),
- cloud CLIs, kubeconfigs, and any credentials already set up on the box.

The MCP client never sees your keys or tokens — it just delegates a task to a
worker that already has access.

```
MCP client (e.g. Claude Desktop)
        │  MCP over stdio
        ▼
  cli-agent-mcp                      ← this project
        │  spawn (headless)
        ▼
  claude / cursor-agent / your CLI   ← inherits env: VPN, SSH agent, credentials
        │
        ▼
  your code, your servers
```

## Design

Rather than screen-scraping an interactive TUI (fragile: pseudo-terminals, ANSI
codes, prompt detection), each agent runs in **headless / print mode** streaming
newline-delimited output. That gives a clean programmatic contract:

- a **session id** to resume for follow-up turns, and
- a terminal **`result`** event — with the **process exit code** as a universal
  backstop — as the unambiguous "task is done" signal.

### Two modes

- **Seamless / streaming (preferred).** `agent_run_task` blocks until the worker
  finishes and streams **MCP progress notifications** (`notifications/progress`)
  as it works:
  - assistant text as it's written,
  - `⚙ using Bash` / `⚙ using Edit` when the worker invokes a tool,
  - `↳ <last output line>` when that tool returns (`↳ ✗ …` on error),
  - `⚠ …` for anything the agent writes to stderr,
  - `✓ <result>` at the end.

  The client shows these live and progress resets its timeout, so long tasks stay
  alive. The final result comes back inline. No polling.
- **Parallel / fire-and-forget.** `agent_start_task` returns a `task_id`
  immediately and runs in the background, so you can launch several workers at
  once and collect them with `agent_task_status` / `agent_get_output` later.

## Tools

| Tool | Purpose |
|------|---------|
| **`agent_run_task`** | **Preferred.** Delegate a task and wait, streaming live progress; returns the result inline. |
| **`agent_plan_task`** | Have the agent **propose** a plan without executing anything. Use before risky work, then follow up to execute. |
| **`agent_run_followup`** | Continue a session and wait, with the same live streaming. |
| `agent_start_task` | Delegate without waiting; returns a `task_id` (background). For parallel/fire-and-forget. |
| `agent_task_status` | Poll status (`running`/`done`/`failed`/`canceled`) and read the result of a backgrounded task. |
| `agent_get_output` | Fetch the streamed transcript (supports incremental `since_line`/`max_lines`). |
| `agent_send_followup` | Non-blocking follow-up on a backgrounded task. |
| `agent_cancel_task` | Terminate a running task. |
| `agent_list_tasks` | List all tasks, newest first. |
| `agent_list_agents` | Show which agents are available on this machine. |

## Install

### Download a binary (recommended)

Grab a prebuilt binary from the
[**Releases**](https://github.com/andresh0816/cli-agent-mcp/releases/latest) page.
The assets are raw binaries — no unzip needed.

| Platform | Asset |
|----------|-------|
| Windows (x64) | `cli-agent-mcp_windows_amd64.exe` |
| Windows (ARM) | `cli-agent-mcp_windows_arm64.exe` |
| macOS (Apple Silicon) | `cli-agent-mcp_darwin_arm64` |
| macOS (Intel) | `cli-agent-mcp_darwin_amd64` |
| Linux (x64) | `cli-agent-mcp_linux_amd64` |
| Linux (ARM) | `cli-agent-mcp_linux_arm64` |

`checksums.txt` (SHA-256) is attached to every release to verify the download. On
macOS/Linux remember to `chmod +x` the downloaded file.

### With Go

```bash
go install github.com/andresh0816/cli-agent-mcp@latest
```

### From source

```bash
git clone https://github.com/andresh0816/cli-agent-mcp
cd cli-agent-mcp
go build -o cli-agent-mcp .
```

Check what it can drive on your machine:

```bash
cli-agent-mcp --list-agents
cli-agent-mcp --help
```

## Configure your MCP client

Point your client at the binary. For **Claude Desktop**, edit
`claude_desktop_config.json` (Windows: `%APPDATA%\Claude\`, macOS:
`~/Library/Application Support/Claude/`):

```json
{
  "mcpServers": {
    "cli-agent": {
      "command": "/absolute/path/to/cli-agent-mcp",
      "env": {
        "CLI_AGENT_MCP_DEFAULT_AGENT": "claude",
        "CLI_AGENT_MCP_DEFAULT_CWD": "/absolute/path/to/your/project",
        "CLI_AGENT_MCP_PERMISSION_MODE": "acceptEdits"
      }
    }
  }
}
```

On Windows, use double backslashes:

```json
"command": "C:\\Tools\\cli-agent-mcp.exe",
"env": { "CLI_AGENT_MCP_DEFAULT_CWD": "C:\\code\\my-project" }
```

Restart the client, and the `cli-agent` tools appear. Then ask it something like
*"use the cli-agent to run the test suite and tell me what fails."*

> The worker agent must be installed and **authenticated** on its own
> (e.g. run `claude` or `cursor-agent` once interactively to log in).

## Configuration

All configuration is environment variables, so it lives entirely in your client's
`mcpServers` entry.

| Variable | Default | Meaning |
|----------|---------|---------|
| `CLI_AGENT_MCP_DEFAULT_AGENT` | `claude` | Agent used when a call omits `agent`. |
| `CLI_AGENT_MCP_CLAUDE_BIN` | `claude` | Claude Code launcher (name in PATH or absolute path). |
| `CLI_AGENT_MCP_CURSOR_BIN` | `cursor-agent` | Cursor launcher, used if the bundled runtime isn't auto-detected. |
| `CLI_AGENT_MCP_PERMISSION_MODE` | `acceptEdits` | Claude Code `--permission-mode`: `acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan`. |
| `CLI_AGENT_MCP_ALLOWED_TOOLS` | — | Claude Code `--allowedTools` allowlist, patterns supported (e.g. `Bash(git *),Edit`). **The best way to bound a headless worker.** |
| `CLI_AGENT_MCP_DISALLOWED_TOOLS` | — | Claude Code `--disallowedTools` denylist. |
| `CLI_AGENT_MCP_ALLOW_EXTRA_ARGS` | `false` | Allow callers to pass raw agent flags via `extra_args`. **Keep off** — see safety. |
| `CLI_AGENT_MCP_CLAUDE_EXTRA_ARGS` | — | Extra Claude flags, `;`-separated. |
| `CLI_AGENT_MCP_CURSOR_EXTRA_ARGS` | — | Extra Cursor flags, `;`-separated. |
| `CLI_AGENT_MCP_DEFAULT_CWD` | server's cwd | Working directory when a call omits `cwd`. **Set this.** |
| `CLI_AGENT_MCP_ALLOWED_CWDS` | — | If set, every task `cwd` must live under one of these roots (`;`-separated). |
| `CLI_AGENT_MCP_MAX_TASKS` | `100` | Max retained tasks in memory. |
| `CLI_AGENT_MCP_CUSTOM_BIN` | — | Executable for the custom agent (see below). |
| `CLI_AGENT_MCP_CUSTOM_ARGS` | — | Argument template for the custom agent, `;`-separated. |
| `CLI_AGENT_MCP_CUSTOM_NAME` | `custom` | Name to expose the custom agent as. |

## Drive any CLI agent (no code)

The built-in `custom` adapter runs **any** command-line agent. Give it a binary
and an argument template; these placeholders are substituted per run:

| Placeholder | Value |
|-------------|-------|
| `{{prompt}}` | the task text |
| `{{cwd}}` | the working directory |
| `{{model}}` | the model override (may be empty) |
| `{{session}}` | the session id when resuming (empty on the first turn) |

**Rule:** if an argument's placeholder expands to an empty value, that whole
argument is dropped. So write optional flags in the single-argument
`--flag=value` form (e.g. `--model={{model}}`) — the flag then disappears
cleanly instead of leaving a dangling `--model`.

```json
{
  "mcpServers": {
    "cli-agent": {
      "command": "/absolute/path/to/cli-agent-mcp",
      "env": {
        "CLI_AGENT_MCP_DEFAULT_AGENT": "aider",
        "CLI_AGENT_MCP_CUSTOM_NAME": "aider",
        "CLI_AGENT_MCP_CUSTOM_BIN": "aider",
        "CLI_AGENT_MCP_CUSTOM_ARGS": "--no-pretty;--yes;--message;{{prompt}}"
      }
    }
  }
}
```

Output handling is deliberately forgiving: JSON lines are parsed tolerantly (a
recognized session id enables follow-ups, a recognized terminal event sets the
result), plain-text lines stream as progress, completion comes from the exit
code, and since most simple CLIs just print their answer, the collected output
becomes the task result.

For a first-class integration (session resume, rich tool events), implement the
small `agent.Adapter` interface in [`internal/agent`](internal/agent/adapter.go)
— see `claude.go` for a fully-featured example. PRs welcome.

## ⚠️ Permissions & safety

Read this section before pointing the server at anything you care about.

### The client cannot stop the worker

Once a task starts, **the MCP client is a spectator, not a gatekeeper.** Progress
notifications are informational and arrive *after* each step has already run —
there is no approval hook, and the calling model is blocked awaiting the result
rather than watching. In normal CLI use *you* are the approval gate; running the
agent headless removes that gate. Nothing replaces it automatically.

So the controls that matter are the ones you configure **here**, plus planning.

### Plan first for anything risky

`agent_plan_task` runs the agent in plan-only mode: it inspects and proposes,
but **executes nothing**. Review the plan, then call `agent_run_followup` with
the returned `task_id` to carry it out. This turns *fire-and-pray* into
*propose → review → execute*, and is the only way to get judgment in the loop
before an action happens.

It **fails closed**: agents that can't guarantee plan-only (`cursor`, `custom`)
refuse the call rather than executing. `agent_list_agents` reports
`supports_plan_only` per agent.

### Bound what the worker may do

Prefer a precise allowlist over a permissive mode:

```json
"CLI_AGENT_MCP_ALLOWED_TOOLS": "Read,Grep,Glob,Edit,Bash(git *),Bash(npm test)"
```

`--allowedTools` / `--disallowedTools` support patterns, so you can grant exactly
`Bash(git *)` instead of choosing between *no commands at all* and *every
command*. This is server-side policy — tool callers cannot override it.

For Claude Code, `--permission-mode` (`CLI_AGENT_MCP_PERMISSION_MODE`) accepts
`acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan`:

- `acceptEdits` (default) — auto-approves file edits, but **commands still need
  approval**, so a task that must run commands may stall or skip them.
- `bypassPermissions` — runs everything, no prompts. Often what's wanted for
  *"log into the server and run X"*, **but it means one AI can make another AI
  run arbitrary commands on your machine and infrastructure.** If you need this,
  pair it with `CLI_AGENT_MCP_ALLOWED_TOOLS` rather than leaving it wide open.

### `extra_args` is disabled by default — keep it that way

The `extra_args` tool parameter appends raw flags to the agent, *after* the flags
configured above. Left open, a caller could pass
`--dangerously-skip-permissions` and void your entire policy. Since the caller is
itself a model — one that may be reading untrusted web pages, issues, or emails —
it is off by default and calls using it are refused. Only set
`CLI_AGENT_MCP_ALLOW_EXTRA_ARGS=true` if you trust the caller as much as your own
shell.

### Prompt injection is the sharp edge

If the orchestrating model processes untrusted content, that content can shape
the prompt it delegates — and the worker itself reads files and pages that may
carry injected instructions. The blast radius is whatever the host machine can
reach, including private infrastructure. Bound it deliberately:

- `CLI_AGENT_MCP_ALLOWED_CWDS` — restrict where tasks may run.
- `CLI_AGENT_MCP_ALLOWED_TOOLS` — restrict what they may do.
- `CLI_AGENT_MCP_DEFAULT_CWD` — pin a specific project.
- Plan first; keep `extra_args` off.

## Development

```bash
go build ./...        # compile
go vet ./...          # static checks
go test ./...         # unit tests (incl. permission/flag construction)
gofmt -l .            # formatting (must be empty)

# End-to-end test over real MCP stdio using the built-in mock agent —
# needs no Claude Code or Cursor installed:
go build -o cli-agent-mcp .
go run ./cmd/smoketest ./cli-agent-mcp
```

The smoke test is env-driven, so you can point it at a real agent:

```bash
SMOKE_AGENT=claude SMOKE_PROMPT="say hi" SMOKE_FOLLOWUP=0 \
  go run ./cmd/smoketest ./cli-agent-mcp
```

CI ([`ci.yml`](.github/workflows/ci.yml)) runs all of the above on every push and PR.

## Releases

Releases are automated by [`release.yml`](.github/workflows/release.yml). Cutting
one is a single tag push:

```bash
git tag v0.2.0
git push origin v0.2.0
```

That cross-compiles every platform binary (version baked in via
`-ldflags -X main.version=...`), writes `checksums.txt`, and publishes a GitHub
Release with auto-generated changelog notes and all assets attached. It can also
be run manually from the Actions tab against an existing tag.

## Project layout

```
main.go                     entry point, MCP tool wiring, __mock subcommand
internal/config/            env-var configuration
internal/agent/
  adapter.go                Adapter interface + registry + exec helper
  claude.go                 Claude Code adapter
  cursor.go                 Cursor adapter (bundled-runtime detection)
  custom.go                 generic, env-configured adapter for any CLI
  mock.go                   built-in mock agent
  streamjson.go             Claude/mock stream-json parser
  tolerant.go               schema-tolerant parser for other agents
internal/task/manager.go    task manager (spawn, pump, complete, resume, stream)
cmd/smoketest/              end-to-end test via the MCP client
```

## Contributing

Issues and PRs are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
