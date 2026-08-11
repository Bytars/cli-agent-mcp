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
- **Director / supervised.** Start in the background, then loop `agent_watch`
  (a long-poll that returns the moment new output arrives) so the orchestrating
  model reads the transcript *as it happens* and can `agent_cancel_task` the
  instant it drifts. This is the closest thing to "the model watches over the
  worker's shoulder and stops it." See [Director mode](#director-mode-supervise--interrupt).

## Tools

| Tool | Purpose |
|------|---------|
| **`agent_run_task`** | **Preferred.** Delegate a task and wait, streaming live progress; returns the result inline. |
| **`agent_plan_task`** | Have the agent **propose** a plan without executing anything. Use before risky work, then follow up to execute. |
| **`agent_run_followup`** | Continue a session and wait, with the same live streaming. |
| `agent_start_task` | Delegate without waiting; returns a `task_id` (background). For parallel or supervised use. |
| **`agent_watch`** | Long-poll a backgrounded task: blocks until new output or completion, returns the new lines. The supervise-and-interrupt primitive. |
| **`agent_task_board`** | Open the live task board — an interactive panel that keeps refreshing on its own. See [Seeing where tasks stand](#seeing-where-tasks-stand). |
| `agent_task_status` | Poll status (`running`/`done`/`failed`/`canceled`) and read the result of a backgrounded task. |
| `agent_get_output` | Fetch the streamed transcript (supports incremental `since_line`/`max_lines`). |
| `agent_send_followup` | Non-blocking follow-up on a backgrounded task. |
| **`agent_task_diff`** | Show what a task actually changed: files, line counts, commits, optionally the patch. The agent's summary is a claim; this is the evidence. |
| `agent_remove_worktree` | Delete the isolated checkout a task ran in. Refuses while it still holds uncommitted work. |
| `agent_cancel_task` | Terminate a running task. |
| `agent_list_tasks` | List all tasks, newest first. |
| `agent_list_agents` | Show which agents are available on this machine. |
| `agent_diagnose` | Why an agent fails with no explanation: package identity, whether spawning works, how each launcher resolves, whether permission questions can reach you. |

## What a task cost

Delegating hides everything a person at a terminal would have watched accumulate,
and cost is the part that compounds without anyone noticing. Every result now
carries what the run cost, the tokens it moved, how many turns it took and which
model actually ran — on the board, in the audit log, and inline with the answer:

```
Task task-3-9b8b5e97 finished with status "done".

Added the retry with backoff and a test for it.

— $0.1104 · 4 in / 252 out tokens · 56.3k cached · 2 agent turn(s) · 9s · claude-opus-5
```

A run that reported nothing shows nothing. An absent figure and a zero one mean
different things, and printing `$0.0000` for the first would be a lie.

## Reviewing what the worker changed

`agent_task_diff` compares against the commit the repository was on when the task
started, captured up front because it cannot be recovered afterwards: once the
worker commits its own work, a diff against `HEAD` shows nothing and reads as
"the agent changed no files". Untracked files are listed too — a file created and
never staged has still changed the repository, and never appears in a diff.

## Running tasks in parallel without collisions

Agents edit files. Two of them working in the same checkout overwrite each other
and produce a diff neither intended — the failure that arrives the moment
delegating more than one task at a time starts working well enough to be worth
doing.

Pass `isolate: true` and the task gets a git worktree on a branch of its own,
sharing the repository's history so the result still merges normally:

```
agent_run_task { prompt: "…", cwd: "C:\\code\\api", isolate: true }
```

It is opt-in per call rather than a mode. A worktree is a fresh checkout, so
anything the repository does not track — `node_modules`, a `.env`, a build cache
— is simply not there, and a task that needs those has to run in place.

Concurrency is capped at `CLI_AGENT_MCP_MAX_CONCURRENT` (default 3) live workers.
That is a different limit from `MAX_TASKS`, which only bounds retained records:
the current Claude Code binary is a quarter of a gigabyte per worker, so an
orchestrator fanning out ten tasks can exhaust the machine with every other limit
still satisfied.

## Letting the worker ask instead of stalling

A headless agent executes what it is allowed to and asks about the rest. With no
terminal there is nobody to answer, so the run either hangs or the operator
pre-approves everything up front — trading the ability to say no for the ability
to finish.

When the client supports it, this server takes the third option: Claude Code is
launched with `--permission-prompt-tool` pointing at a single-use endpoint on
loopback, and each request becomes a question put to the person who delegated the
task, showing the actual command rather than "may the agent use Bash". A refusal
reaches the agent as a reason it can work around. The grant is revoked with its
config file the moment the turn ends.

**It does not engage for every client.** It needs one that declared the
elicitation capability, *and* a negotiated protocol older than `2026-07-28` —
from that version the spec ([SEP-2322](https://blog.modelcontextprotocol.io/posts/2026-07-28/))
forbids a server from opening an elicitation on its own and requires the question
to be embedded in the result of a call the client itself made. A worker's
permission request does not arrive during such a call, so there is nothing to
attach it to.

When it cannot engage, behaviour is exactly what it was before — the operator's
pre-approved list decides — and `agent_diagnose` says so in as many words:

```
ask to run  : no — MCP 2026-07-28 no longer lets a server open an elicitation on its own (SEP-2322) …
              A tool that is neither pre-approved nor denied will stall instead of asking;
              pre-approve what tasks need with CLI_AGENT_MCP_ALLOWED_TOOLS or allowed_tools.
```

## One server, several clients

Over stdio the client owns the process, so every client that wants these tools
starts its own copy — and a second copy cannot see, watch or cancel the first
one's workers. That is what the orphaned-instance machinery below exists to
survive.

`--http` removes the cause instead:

```bash
cli-agent-mcp --http 127.0.0.1:7777
```

One long-lived server, one task registry, and every client pointed at
`http://127.0.0.1:7777/mcp`. A socket has no parent to inherit trust from and
this process can run arbitrary commands on the machine, so it binds to loopback
and requires a bearer token (`CLI_AGENT_MCP_HTTP_TOKEN`, or one is generated and
printed to stderr at startup). A non-loopback bind is allowed and says so loudly
in the log.

## Seeing where tasks stand

Progress notifications only exist while a tool call is in flight. `agent_run_task`
and `agent_watch` stream them, but once `agent_start_task` returns there is
nothing left to watch — which is why a backgrounded task can feel like it
vanished.

`agent_task_board` is the answer to that. It is an
[MCP App](https://modelcontextprotocol.io/extensions/apps/overview): the server
ships an HTML view at `ui://cli-agent-mcp/task-board.html`, the host renders it
in a sandboxed iframe inside the conversation, and the view calls this server's
own tools on its own schedule. So it keeps updating after the call that opened
it has already returned — one row per task with its status, elapsed time and
live transcript, and a cancel button on anything still running. It polls every
2s while something runs, backs off to 8s once everything has settled, and pauses
while the panel is off-screen.

**Host support.** This needs a host implementing the MCP Apps extension (spec
`2026-01-26`). The server advertises it during `initialize` under
`capabilities.extensions["io.modelcontextprotocol/ui"]`. Hosts that don't
implement it ignore the view and render the same listing as plain text, so the
tool is safe to call anywhere.

Interactive views are documented for Claude, Cowork, Claude Desktop and mobile.
Note, though, that Anthropic's guidance covers **remote connectors** and does not
state whether a local stdio server like this one gets its views rendered — and
there is an [open report](https://github.com/modelcontextprotocol/ext-apps/issues/671)
of hosts negotiating the capability but not rendering the iframe. If the panel
never appears, that is why: you get the text listing, and `agent_watch` stays the
reliable way to follow a task live.

## Following a long task without getting cut off

Clients cap how long they will wait on a tool call — Claude Desktop at 60
seconds. `agent_watch` used to block until the task finished, which meant that
on anything longer the client abandoned the call and **discarded the result the
server was about to return**. From the user's side the watch simply stopped,
with nothing to show for it.

`agent_watch` now blocks for at most `CLI_AGENT_MCP_WATCH_WINDOW_SECONDS`
(default 50) and then returns what it has, with `running: true` and a
`next_since_line`. That is not a failure and the task is not interrupted: the
caller repeats the call with that `since_line` until `running` is false. The
tool description and the server instructions both say so, because a bounded
watch that the model reads as "finished" would be no better than the timeout.

Set the window below your client's limit if it differs, or pass
`timeout_seconds` on an individual call.

## Surviving a restart — and a second instance

MCP clients start server processes; they do not always stop the old one first.
When that happened, the new process came up with an empty in-memory registry:
`agent_list_tasks` returned nothing and every previous `task_id` was unknown,
while the workers those tasks owned kept running under the original process and
kept writing their results to disk. Nothing was broken except the server's
ability to see its own work.

Two things close that gap.

**The task registry is on disk.** Each task is written to
`<state-dir>/tasks/<task-id>.json` as it progresses, with its transcript
appended to `<task-id>.log` line by line — as the lines arrive, so a run still
in flight is readable too. On startup the server loads them back, so listing,
reading output and reading results all work across restarts. Records are pruned
to `CLI_AGENT_MCP_MAX_TASKS`, newest kept.

A task the *previous* process was still running comes back with status
**`orphaned`** rather than `running`. This process cannot watch it, cancel it,
or learn how it ended — the worker may have finished long ago, or may still be
going under the old instance. Calling it `running` would make `agent_watch`
block forever on something that can never be seen to finish. Follow-ups on an
orphaned task are refused for the same reason: resuming would put a second
worker on a session the old instance may still be driving.

**A PID lock detects the second instance.** The server records itself in
`<state-dir>/server.lock`. If it finds a lock naming a process that is still
alive, every task listing carries a warning naming that PID. It does not refuse
to start — the client that just launched this process is talking to it and
nothing else, so failing would leave you with no server at all, which is worse
than having two.

> If you stop the older process, be aware that any worker still running under it
> dies with it: workers are held in a Job Object with `KILL_ON_JOB_CLOSE`, so
> closing the owner terminates them mid-write. Let them finish first.

State lives in `%AppData%\cli-agent-mcp` on Windows and `~/.config/cli-agent-mcp`
elsewhere, unless `CLI_AGENT_MCP_STATE_DIR` says otherwise.

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
| `CLI_AGENT_MCP_DISALLOWED_TOOLS` | — | Claude Code `--disallowedTools` (patterns, e.g. `Bash(rm:*),Bash(git push:*)`). **The reliable deny gate for a headless worker.** |
| `CLI_AGENT_MCP_ALLOWED_TOOLS` | — | Claude Code `--allowedTools` — pre-approves tools; **additive, does not restrict.** |
| `CLI_AGENT_MCP_ALLOW_EXTRA_ARGS` | `false` | Allow callers to pass raw agent flags via `extra_args`. **Keep off** — see safety. |
| `CLI_AGENT_MCP_APPEND_SYSTEM_PROMPT` | — | Standing guidance added to every task's system prompt (Claude `--append-system-prompt`). See [Windows + 1Password SSH](#windows--1password-ssh). |
| `CLI_AGENT_MCP_CLAUDE_EXTRA_ARGS` | — | Extra Claude flags, `;`-separated. |
| `CLI_AGENT_MCP_CURSOR_EXTRA_ARGS` | — | Extra Cursor flags, `;`-separated. |
| `CLI_AGENT_MCP_DEFAULT_CWD` | server's cwd | Working directory when a call omits `cwd`. **Set this.** |
| `CLI_AGENT_MCP_ALLOWED_CWDS` | — | If set, every task `cwd` must live under one of these roots (`;`-separated). |
| `CLI_AGENT_MCP_MAX_TASKS` | `100` | Max retained tasks in memory. |
| `CLI_AGENT_MCP_MAX_CONCURRENT` | `3` | Max workers running at once. Separate from `MAX_TASKS`, which only bounds retained records. |
| `CLI_AGENT_MCP_ASK_PERMISSION` | `true` | Let a worker ask you before running something it isn't pre-approved for. Only engages for clients that can be asked — see above. |
| `CLI_AGENT_MCP_PERMISSION_TIMEOUT_SECONDS` | `600` | How long a worker waits for that answer before the request is denied. |
| `CLI_AGENT_MCP_WORKTREE_DIR` | `<state dir>/worktrees` | Where isolated task checkouts are created. |
| `CLI_AGENT_MCP_HTTP_TOKEN` | generated | Bearer token required by `--http`. Printed to stderr when generated. |
| `CLI_AGENT_MCP_AUDIT_LOG` | — | Path to a JSONL audit log of what the worker did. See [Audit log](#audit-log). |
| `CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS` | `0` (off) | Kill a turn that runs longer than this — a safety net for a worker hung on a permission prompt. |
| `CLI_AGENT_MCP_COMPACT` | `true` | `agent_get_output`/`agent_watch` return a filtered, readable transcript instead of raw JSONL (pass `raw: true` on a call to override). |
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

## Director mode: supervise & interrupt

The natural question is: *can the orchestrating model (e.g. Claude Desktop)
watch the worker and stop it if it goes wrong, the way a human driving the CLI
would?* Yes — but **not inside a blocking `agent_run_task` call.** During that
call the model is suspended awaiting the result; the progress notifications go to
the human's UI, not into the model's reasoning, so it can't intervene.

To put the model in the director's seat, run the task in the background and let
it supervise:

1. `agent_start_task` → get a `task_id` (returns immediately).
2. Loop `agent_watch` with the `task_id`, passing back the `next_since_line` it
   returns each time. `agent_watch` blocks until new output arrives (or the task
   ends), so the model reads the transcript *as it happens*. **Between calls the
   model is active and reasoning** — that's where it judges whether the worker is
   on track.
3. If it drifts, `agent_cancel_task` stops it immediately. On Windows the turn
   runs inside a Job Object created with `KILL_ON_JOB_CLOSE`, and on Unix in its
   own process group, so cancelling reaches the tree the worker created — not
   just the launcher, and including grandchildren whose intermediate parent has
   already exited, which is where `taskkill /T` used to lose them.

   The boundary, measured rather than assumed: a process the worker *detaches*
   through ShellExecuteEx — PowerShell's `Start-Process` does this by default —
   is created by the shell rather than by us. It never joins the job, and no
   parentage-based mechanism can reach it. In testing, such a grandchild was
   still alive and writing after its turn ended. Cancellation is containment of
   the worker, not a guarantee that nothing it ever launched can outlive it.

The server's tool instructions teach this flow, so a capable client will do it on
its own when supervision matters.

**Honest limits.** This is *observe-and-interrupt*, not *pre-approve*: the model
reacts to a step after seeing it in the transcript, and interruption stops what's
next — it can't undo what already ran. And because a worker turn runs to
completion, you can't inject a prompt mid-turn; you steer *between* turns (cancel
and restart with a corrected prompt, or `agent_send_followup`). That mirrors how
a human drives one of these agents by hand. For anything destructive, combine
this with `agent_plan_task` so judgment happens *before* execution.

## Audit log

Set `CLI_AGENT_MCP_AUDIT_LOG` to a file path to record an append-only JSONL trail
of everything the worker was asked to do — useful when a headless agent can reach
real infrastructure:

```json
"CLI_AGENT_MCP_AUDIT_LOG": "/var/log/cli-agent-mcp/audit.jsonl"
```

Each line is one event:

- `turn_start` — task id, agent, cwd, the prompt, and the **exact command line
  executed** (so you can see which permission mode and tool policy were applied).
- `tool_use` — each tool the worker invoked, with its input (e.g. the actual
  shell command it ran).
- `tool_result` — the outcome of each tool call (`is_error` + output). A command
  that failed with **no output** (e.g. an executable killed by security software)
  is recorded explicitly rather than vanishing, and shows up in the transcript as
  `↳ ✗ (failed with no output — possibly blocked by security software / sandbox)`.
- `turn_end` — status, exit code, duration, and a snippet of the result.
- `cancel` — when a task was interrupted.

```json
{"ts":"2026-07-16T02:22:26Z","event":"turn_start","task_id":"task-1-…","agent":"claude","cwd":"/code/app","prompt":"run the tests","command":["claude","-p","run the tests","--output-format","stream-json","--verbose","--permission-mode","acceptEdits"]}
{"ts":"2026-07-16T02:22:31Z","event":"tool_use","task_id":"task-1-…","tool":"Bash","input":"{\"command\":\"npm test\"}"}
{"ts":"2026-07-16T02:22:44Z","event":"turn_end","task_id":"task-1-…","status":"done","exit_code":0,"duration_ms":13000}
```

## The inherited environment — read this before debugging a silent failure

An MCP client launches this server as a child process, and some clients hand it
a **curated environment** rather than the one a login shell would have. Claude
Desktop on Windows is one: the environment it passes omits `ProgramData`,
`ComSpec`, `OS`, `COMPUTERNAME` and `SESSIONNAME`, and reduces `PATHEXT` to
`.CPL`.

That looks cosmetic. It is not.

Microsoft's Win32 build of OpenSSH resolves its system configuration directory
from `%ProgramData%` during platform initialisation — before it parses arguments
and before its logging subsystem exists. With the variable absent, `ssh.exe`
exits **255 having written nothing to stdout or stderr**. Not even `ssh -V`
prints its banner, and `-E logfile` produces no file. The identical binary works
perfectly from an interactive shell.

This was measured, not guessed. Adding `ProgramData` alone flips the exit code
from 255 to 0; adding `ALLUSERSPROFILE` alone does not. Before that was found,
four plausible explanations had to be eliminated one at a time — MSIX package
identity, a missing console, code-signing policy, and the runtime doing the
spawning. All four were wrong. A silent failure gives you nothing to reason
from, so it invites confident stories that fit the symptoms and miss the cause.

Two consequences shaped the design here:

**The server repairs the environment before spawning.** `agent.RepairedEnviron`
restores well-known system variables that are missing, then the task manager
passes that to the worker. It never overrides a variable the host did set, and
it only injects a path after confirming that path exists — a wrong guess becomes
a no-op instead of a new failure mode. Whatever it had to restore is recorded in
the `turn_start` audit event.

**`agent_diagnose` reports the hole directly.** It lists which standard
variables the launching client failed to pass, alongside package identity, spawn
probes, and how each agent resolves its binary. When a worker fails without
explaining itself, that is the first call to make — it turns hours of
elimination into one tool call.

`PATHEXT` deserves a note of its own: with a reduced value, PATH look-ups stop
finding executables. `where ssh` reports nothing even when `ssh.exe` is sitting
on `PATH`, which is a very effective way to send an investigation in the wrong
direction.

### Script shims are resolved, not shelled

`npm i -g @anthropic-ai/claude-code` installs `claude.cmd` on Windows: a batch
script whose only purpose is to find Node and hand it a `.js` file. Running that
through `cmd /c` is avoided because of quoting. Go escapes arguments for
`CommandLineToArgvW`, but `cmd.exe` parses with different rules and does not
recognise `\"`. A prompt containing a double quote could therefore close the
quoted region early and let the remainder be read as shell syntax — and prompts
here are model-generated, so that is reachable input. Go's CVE-2024-24576
mitigation does not cover this shape: it triggers when the *target* is a
`.bat`/`.cmd`, and here the target is `cmd.exe` itself.

So the server reads the shim, extracts the Node runtime and entry point it would
have used, and runs `node.exe entry.js …` directly — one process, argv passed
verbatim, no shell parser anywhere. `agent_diagnose` shows this as
`shim_resolved`. When a launcher cannot be resolved this way and an argument
contains a character that could not survive a second parser (`"`, `%`, `!`), the
run is refused with an actionable message rather than executed hopefully.

## Windows + 1Password SSH

A gotcha worth documenting, since delegating internal-server work over SSH is a
core use case. On Windows there are two OpenSSH clients:

- **Windows OpenSSH** (`C:\Windows\System32\OpenSSH\ssh.exe`) — talks to the
  1Password SSH agent over its named pipe. **This one works.**
- **Git-bash / MSYS OpenSSH** (`/usr/bin/ssh`) — uses Unix-socket agent
  semantics and **cannot open the Windows named pipe**, so it never sees your
  1Password keys (`get_agent_identities: ... No such file or directory`).

The catch: a worker's `Bash` tool runs `bash -c`, where a bare `ssh` resolves to
**git-bash's** client → no keys → `Permission denied (publickey,password)`, even
though `ssh` works fine in a normal terminal. (`IdentityAgent \\.\pipe\...` in
`~/.ssh/config` does **not** fix git-bash ssh — MSYS can't open that pipe.)

Fix: tell the worker to always use the full Windows path. Set once, applies to
every task:

```json
"CLI_AGENT_MCP_APPEND_SYSTEM_PROMPT": "For any SSH/scp to internal servers, always invoke the Windows OpenSSH client by its full path C:\\Windows\\System32\\OpenSSH\\ssh.exe (and scp.exe) — the bare 'ssh' resolves to git-bash OpenSSH which cannot reach the 1Password SSH agent. Use -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new."
```

Verified: with this in place, a plain task like *"connect to root@bastion and
report its hostname"* makes the worker reach for the Windows client and
authenticate through 1Password with no further hand-holding.

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

**A headless worker executes tool calls by default** — this was verified against
Claude Code 2.1.207: a `-p` run in `default` or `acceptEdits` mode runs shell
commands without prompting (there's no human to prompt). So you do **not** need
`bypassPermissions` to get real work done, and — importantly — you cannot rely on
a permission mode to hold it back.

The reliable brake is the **denylist**:

```json
"CLI_AGENT_MCP_DISALLOWED_TOOLS": "Bash(rm:*),Bash(git push:*),Bash(sudo:*)"
```

`--disallowedTools` hard-denies matching tools/commands (a denied call shows up in
the transcript as a `permission_denials` entry). `Bash` blocks all shell use;
`Bash(git push:*)` blocks just that. This is server-side policy — tool callers
cannot override it.

> **`--allowedTools` does _not_ restrict.** It is *additive*: it pre-approves
> tools (removes prompts), but tools not on it still run. Treat it as "run these
> without ceremony," never as "only these are allowed." Verified: with
> `--allowedTools "Bash(git status)"`, the worker still happily ran `git log`.

### Avoid headless stalls: pre-approving tools

A tool the worker hasn't been trusted for yet can **stall a headless run** — it
waits for an approval no one is there to give. Pre-approving via `--allowedTools`
fixes that (that's what pre-approval is *for*). Two ways, neither of which needs
the `extra_args` escape hatch:

- **Operator default (all tasks):** set `CLI_AGENT_MCP_ALLOWED_TOOLS` in the
  server env, e.g. `"PowerShell,Bash,Edit,Read"`.
- **Per task:** pass `allowed_tools` on `agent_run_task` / `agent_start_task` /
  `agent_plan_task` / the follow-ups, e.g. `["Bash(git *)","PowerShell"]`. The
  server merges it with its own allowlist and joins everything into a single
  argument, so a value can never be smuggled in as a CLI flag — and the deny
  policy still wins. This is the safe way for a client to request scoped
  permissions without opening `extra_args`.

Belt and suspenders: set `CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS` so a run that stalls
anyway is killed with a clear "timed out — possibly blocked on a permission
prompt" error instead of hanging forever.

Because a denylist is inherently incomplete, it is one layer — combine it with the
directory boundary, plan-first, director-mode supervision, and the audit log
below. `--permission-mode` (`CLI_AGENT_MCP_PERMISSION_MODE`) still exists
(`acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan`);
`plan` is what powers `agent_plan_task`.

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
- `CLI_AGENT_MCP_DISALLOWED_TOOLS` — deny the dangerous operations.
- `CLI_AGENT_MCP_DEFAULT_CWD` — pin a specific project.
- Plan first, supervise in director mode, keep the audit log on, keep `extra_args` off.

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
internal/task/
  manager.go                task manager (spawn, pump, complete, resume, stream, watch)
  kill_windows.go           process-tree kill on cancel (Windows)
  kill_other.go             process-group kill on cancel (Unix)
internal/audit/audit.go     append-only JSONL audit trail
internal/state/
  state.go                  durable task records + the instance PID lock
  alive_windows.go          is that PID still running? (Windows)
  alive_other.go            is that PID still running? (Unix)
internal/task/persist.go    saving tasks and restoring a previous process's
internal/ui/
  ui.go                     MCP Apps wiring (capability, ui:// resource, tool _meta)
  board.html                the task board view, self-contained (deny-by-default CSP)
cmd/smoketest/              end-to-end test via the MCP client
```

## Contributing

Issues and PRs are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
