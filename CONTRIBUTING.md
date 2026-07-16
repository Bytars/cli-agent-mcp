# Contributing

Thanks for your interest! Issues, ideas, and pull requests are all welcome.

## Getting started

```bash
git clone https://github.com/andresh0816/cli-agent-mcp
cd cli-agent-mcp
go build ./...
```

Requires Go 1.23+ (see `go.mod` for the version CI uses).

## Before opening a PR

Please make sure these pass — they're exactly what CI runs:

```bash
gofmt -l .            # must print nothing
go vet ./...
go build ./...
go test ./...

# End-to-end test over real MCP stdio, using the built-in mock agent.
# Requires no Claude Code / Cursor install.
go build -o cli-agent-mcp .
go run ./cmd/smoketest ./cli-agent-mcp
```

## Adding support for another CLI agent

You may not need to write any code: the `custom` adapter can drive most CLI tools
through `CLI_AGENT_MCP_CUSTOM_BIN` / `CLI_AGENT_MCP_CUSTOM_ARGS` (see the README).

For a first-class integration — session resume, rich tool-use events, precise
result extraction — implement the `agent.Adapter` interface in
[`internal/agent/adapter.go`](internal/agent/adapter.go):

```go
type Adapter interface {
    Name() string
    Available() (ok bool, detail string)
    Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error)
    ParseLine(line string) Event
}
```

Guidelines:

- **Run the agent headless**, streaming line-delimited output. Don't drive an
  interactive TUI.
- **Never set `Dir` or `Env`** on the returned `exec.Cmd` — the task manager owns
  those, and env inheritance is what gives the worker its network/credential access.
- **`ParseLine` must always populate `Raw`.** Everything else is best-effort:
  completion is backstopped by the process exit code, so a partially-understood
  schema still works.
- If your agent has no terminal result event, implement the optional
  `ResultFromOutput` interface so its output is used as the task result.
- Only implement the optional `PlanCapable` interface if your agent can *truly*
  propose without executing. It must fail closed: `agent_plan_task` refuses
  agents that don't implement it, because silently executing a task the caller
  asked it to merely plan is the worst possible outcome.
- Anything that bounds what the worker may do (permission modes, tool
  allowlists) belongs in server config — never in a parameter the calling model
  can set, since the caller is itself a model.
- Register the adapter in `main.go` and add any config to `internal/config`.

Please include a short note in the README table and, where practical, extend
`internal/agent/mock.go` so the behavior is covered by the smoke test.

## Reporting bugs

Include:

- what you ran (`cli-agent-mcp --version`, `--list-agents` output),
- your OS and the agent you're driving,
- the relevant server log (it goes to **stderr**; stdout is the MCP wire),
- and the task transcript from `agent_get_output` if you have it.

## Code of conduct

Be respectful and constructive. That's it.
