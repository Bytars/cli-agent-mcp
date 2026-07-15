# cli-agent-mcp

[![ci](https://github.com/andresh0816/cli-agent-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/andresh0816/cli-agent-mcp/actions/workflows/ci.yml)
[![release](https://github.com/andresh0816/cli-agent-mcp/actions/workflows/release.yml/badge.svg)](https://github.com/andresh0816/cli-agent-mcp/actions/workflows/release.yml)
[![latest release](https://img.shields.io/github/v/release/andresh0816/cli-agent-mcp?sort=semver)](https://github.com/andresh0816/cli-agent-mcp/releases/latest)

An **MCP (Model Context Protocol) stdio server** — a single `.exe`, in the spirit
of [github-mcp-server](https://github.com/github/github-mcp-server) — that lets
an orchestrating client such as **Claude Desktop** drive a **local headless CLI
coding agent** (Claude Code or Cursor) as a background worker.

Instead of manually copy-pasting between Claude Desktop and your terminal agent,
Claude delegates a task, the worker runs it on your machine, and Claude polls for
completion, reads the result, and sends follow-ups — automatically.

## Why this works so well for internal/VPN access

The worker agent runs as a **child process of this server and inherits its full
environment**. So your **VPN routes** and the **1Password SSH agent**
(`SSH_AUTH_SOCK`) are transparently visible to the worker. Claude Desktop never
touches your SSH keys; it just delegates a task to a worker that already has
access to your internal, externally-unreachable servers.

```
Claude Desktop ──(MCP/stdio)──► cli-agent-mcp.exe ──(spawn, headless)──► claude / cursor-agent
                                                                          │ inherits env:
                                                                          │  • VPN
                                                                          │  • 1Password SSH agent
                                                                          ▼
                                                                    internal servers
```

## Design

Rather than screen-scraping an interactive TUI (fragile: pseudo-terminals, ANSI,
prompt detection), each agent is invoked in **headless / print mode** streaming
newline-delimited JSON. That gives a clean programmatic contract:

- a **session id** to resume for follow-up turns, and
- an unambiguous terminal **`result`** event — plus the process exit code — as the
  "task is done" signal.

### Two modes

- **Seamless / streaming (preferred).** `agent_run_task` blocks until the worker
  finishes and streams **MCP progress notifications** (`notifications/progress`)
  as the agent works:
  - assistant text as it's written,
  - `⚙ using Bash` / `⚙ using Edit` when the worker invokes a tool,
  - `↳ <last output line>` when that tool returns (`↳ ✗ …` on error),
  - `✓ <result>` at the end.

  Claude Desktop shows these live and keeps the connection alive (progress resets
  the client timeout), then the final result comes back inline. No polling; it
  feels like Claude did the work itself — like Cowork spawning an agent, but on
  your local machine.
- **Parallel / fire-and-forget.** `agent_start_task` returns a `task_id`
  immediately and runs in the background, so you can launch several workers at
  once and check them with `agent_task_status` / `agent_get_output` later.

### Pluggable adapters

| Agent    | Name       | Launched as | Notes |
|----------|------------|-------------|-------|
| Claude Code | `claude` | `claude -p … --output-format stream-json --verbose` | session resume via `--resume` |
| Cursor      | `cursor` | bundled `node.exe index.js -p … --output-format stream-json` | auto-detects the bundled Node to avoid Windows `.cmd`/`.ps1` quoting |
| Mock        | `mock`   | this exe's hidden `__mock` subcommand | for testing the pipeline with nothing installed |

Adding another agent = implementing the small `agent.Adapter` interface
(`internal/agent/adapter.go`).

## Tools exposed to Claude Desktop

| Tool | Purpose |
|------|---------|
| **`agent_run_task`** | **Preferred.** Delegate a task and wait, streaming live progress; returns the result inline. |
| **`agent_run_followup`** | Continue a session and wait, with the same live streaming. |
| `agent_start_task` | Delegate without waiting; returns a `task_id` (background). For parallel/fire-and-forget. |
| `agent_task_status` | Poll status (`running`/`done`/`failed`/`canceled`) and read the result of a backgrounded task. |
| `agent_get_output` | Fetch the streamed transcript (supports incremental `since_line`/`max_lines`). |
| `agent_send_followup` | Non-blocking follow-up on a backgrounded task. |
| `agent_cancel_task` | Terminate a running task. |
| `agent_list_tasks` | List all tasks, newest first. |
| `agent_list_agents` | Show which agents are available on this machine. |

## Install (recommended: download a binary)

Grab a prebuilt binary from the
[**Releases**](https://github.com/andresh0816/cli-agent-mcp/releases/latest) page —
just like github-mcp-server. Pick the file for your platform:

| Platform | Asset |
|----------|-------|
| Windows (x64) | `cli-agent-mcp_windows_amd64.exe` |
| Windows (ARM) | `cli-agent-mcp_windows_arm64.exe` |
| macOS (Apple Silicon) | `cli-agent-mcp_darwin_arm64` |
| macOS (Intel) | `cli-agent-mcp_darwin_amd64` |
| Linux (x64) | `cli-agent-mcp_linux_amd64` |
| Linux (ARM) | `cli-agent-mcp_linux_arm64` |

`checksums.txt` (SHA-256) is attached to each release to verify the download.

Put the file somewhere stable (e.g. `C:\Tools\cli-agent-mcp.exe`) and point your
Claude Desktop config at it (see below). No unzip needed — the assets are the raw
binaries.

## Build (from source)

Requires Go 1.23+.

```powershell
cd cli-agent-mcp
go build -o cli-agent-mcp.exe .
```

Sanity-check detection and run the end-to-end pipeline test (no agents needed —
uses the built-in mock):

```powershell
.\cli-agent-mcp.exe --list-agents
go run ./cmd/smoketest
```

## Configure Claude Desktop

Edit `%APPDATA%\Claude\claude_desktop_config.json` and add the server under
`mcpServers`. Use the absolute path to the built `.exe`.

```json
{
  "mcpServers": {
    "cli-agent": {
      "command": "C:\\Tools\\cli-agent-mcp.exe",
      "env": {
        "CLI_AGENT_MCP_DEFAULT_AGENT": "claude",
        "CLI_AGENT_MCP_DEFAULT_CWD": "C:\\Users\\anher\\Documents\\Projects\\my-project",
        "CLI_AGENT_MCP_PERMISSION_MODE": "acceptEdits"
      }
    }
  }
}
```

Restart Claude Desktop. You should see the `cli-agent` tools appear. Then just
ask Claude something like *"Use the cli-agent to run the test suite on the
staging server and tell me what fails."*

## Configuration (environment variables)

| Variable | Default | Meaning |
|----------|---------|---------|
| `CLI_AGENT_MCP_DEFAULT_AGENT` | `claude` | Agent used when a call omits `agent`. |
| `CLI_AGENT_MCP_CLAUDE_BIN` | `claude` | Claude Code launcher (name in PATH or absolute path). |
| `CLI_AGENT_MCP_CURSOR_BIN` | `cursor-agent` | Cursor launcher, used only if bundled Node isn't auto-detected. |
| `CLI_AGENT_MCP_PERMISSION_MODE` | `acceptEdits` | Claude Code `--permission-mode`. See the safety note below. |
| `CLI_AGENT_MCP_CLAUDE_EXTRA_ARGS` | — | Extra Claude flags, `;`-separated. |
| `CLI_AGENT_MCP_CURSOR_EXTRA_ARGS` | — | Extra Cursor flags, `;`-separated. |
| `CLI_AGENT_MCP_DEFAULT_CWD` | server's cwd | Working directory when a call omits `cwd`. **Set this.** |
| `CLI_AGENT_MCP_ALLOWED_CWDS` | — | If set, every task `cwd` must live under one of these roots (`;`-separated). |
| `CLI_AGENT_MCP_MAX_TASKS` | `100` | Max retained tasks in memory. |

## ⚠️ Permissions & safety

The worker runs **headless — there is no human at the terminal to approve tool
prompts.** Claude Code's `--permission-mode` decides how autonomous it is:

- `acceptEdits` (default) — auto-approves file edits, but **commands (Bash/SSH)
  still require approval**, so a task that must run commands on an internal server
  may stall or skip them.
- `bypassPermissions` — runs everything without prompting. This is likely what you
  need for *"log into the server and run X"* workflows, **but it means one AI
  (Claude Desktop) can make another AI (the worker) run arbitrary commands on your
  machine and internal infrastructure.** Enable it deliberately.
- `plan` / `default` — more conservative.

Recommended hardening:

- Set `CLI_AGENT_MCP_ALLOWED_CWDS` to restrict where tasks can run.
- Keep `CLI_AGENT_MCP_DEFAULT_CWD` pointed at a specific project.
- Start with `acceptEdits`; only move to `bypassPermissions` once you trust the flow.

## Releases (maintainers)

Releases are automated by [`.github/workflows/release.yml`](.github/workflows/release.yml).
Cutting a release is a single tag push:

```bash
git tag v0.2.0
git push origin v0.2.0
```

That triggers a workflow that cross-compiles every platform binary (with the
version baked in via `-ldflags -X main.version=0.2.0`), writes `checksums.txt`,
and publishes a **GitHub Release** with auto-generated changelog notes and all
assets attached. You can also run it manually from the Actions tab
(`workflow_dispatch`) against an existing tag.

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs `gofmt`, `go vet`,
`go build`, and the end-to-end smoke test on every push and PR.

## Project layout

```
main.go                     entry point, MCP tool wiring, __mock subcommand
internal/config/            env-var configuration
internal/agent/
  adapter.go                Adapter interface + registry + Windows exec helper
  claude.go                 Claude Code adapter
  cursor.go                 Cursor adapter (bundled-node detection)
  mock.go                   built-in mock agent
  streamjson.go             Claude/mock stream-json parser
internal/task/manager.go    async task manager (spawn, pump, complete, resume)
cmd/smoketest/              end-to-end pipeline test via the MCP client
```
