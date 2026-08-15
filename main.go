// SPDX-License-Identifier: Apache-2.0

// Command cli-agent-mcp is a Model Context Protocol server that lets an
// orchestrating client (e.g. Claude Desktop) drive a local headless CLI coding
// agent — Claude Code, Cursor, or any tool you configure — as a background
// worker.
//
// The worker runs on the host machine and inherits this process's environment,
// so whatever that machine can reach (VPN routes, private hosts via an SSH
// agent, credentials) is transparently available to it. The orchestrator
// delegates a task, watches live progress, reads the result, and can send
// follow-up turns — no copy-pasting between two windows.
//
// Transport is stdio, matching how MCP clients launch server binaries.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
	"github.com/Bytars/cli-agent-mcp/internal/audit"
	"github.com/Bytars/cli-agent-mcp/internal/config"
	"github.com/Bytars/cli-agent-mcp/internal/inspect"
	"github.com/Bytars/cli-agent-mcp/internal/pairing"
	"github.com/Bytars/cli-agent-mcp/internal/state"
	"github.com/Bytars/cli-agent-mcp/internal/task"
	"github.com/Bytars/cli-agent-mcp/internal/ui"
)

// instanceWarning is set once during startup, before the server begins serving,
// when another live server process is found holding the state lock. It rides
// along on every task listing because the situation it describes is invisible
// otherwise: the other instance owns tasks this one cannot see, watch or
// cancel, and a caller reading an empty list would conclude nothing is running.
var instanceWarning string

// version is overridden at release time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	// Hidden subcommand used by the built-in mock agent to exercise the full
	// pipeline without Claude Code or Cursor installed.
	if len(os.Args) > 1 && os.Args[1] == "__mock" {
		os.Exit(agent.RunMock(os.Args[2:]))
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Println("cli-agent-mcp", version)
			return
		case "--help", "-h", "help":
			printHelp()
			return
		case "tasks", "logs", "ui":
			// Read-only viewers over the task store. They are a separate process
			// from the server and never touch it: everything a task produces is
			// already on disk while it is producing it, so watching a run live
			// only needs a reader. See internal/inspect.
			log.SetOutput(os.Stderr)
			log.SetPrefix("cli-agent-mcp: ")
			log.SetFlags(0)
			os.Exit(inspect.Run(os.Args[1:]))
		case "pair":
			// Issues the credential that authorizes a client to drive this
			// server. A human runs this at a terminal, so it talks on stdout.
			log.SetOutput(os.Stderr)
			log.SetPrefix("cli-agent-mcp: ")
			log.SetFlags(0)
			os.Exit(pairing.Run(os.Args[2:], state.ResolveDir))
		case "--list-agents":
			// Handled further down, once the registry exists.
		default:
			// Anything else that looks like a flag is a typo, not a request to
			// serve: without this the process would drop into server mode and
			// hang on stdin with no diagnostic. Usage goes to stderr — stdout is
			// the MCP wire.
			if strings.HasPrefix(os.Args[1], "-") {
				fmt.Fprintf(os.Stderr, "cli-agent-mcp: unknown flag %q\n\n", os.Args[1])
				fmt.Fprint(os.Stderr, helpText())
				os.Exit(2)
			}
		}
	}

	// Logs must go to stderr; stdout is the MCP wire.
	log.SetOutput(os.Stderr)
	log.SetPrefix("cli-agent-mcp: ")
	log.SetFlags(0)

	cfg := config.Load()

	// Authorization is settled before anything with a side effect happens.
	// An unauthorized launcher must not reach the instance lock: writing that
	// file would let any local process disturb the legitimate server's view of
	// the tasks it owns, which is a denial of service that needs no token at all.
	stateDir := state.ResolveDir(cfg.StateDir)
	launcher := pairing.ParentExe()
	gate, err := pairing.Verify(stateDir, cfg.Token, launcher)
	if err != nil {
		// Verify still returns a usable verdict on a write failure; the record
		// simply did not get its timestamp or first-use binding updated.
		log.Printf("warning: pairing record: %v", err)
	}
	log.Printf("%s", pairing.StartupLine(stateDir, gate))
	if stderrMsg, _ := pairing.Explain(gate); stderrMsg != "" {
		log.Print(stderrMsg)
	}

	reg := agent.NewRegistry(
		agent.NewClaudeAdapter(cfg.ClaudeBin, cfg.PermissionMode, cfg.AllowedTools, cfg.DisallowedTools, cfg.AppendSystemPrompt, cfg.ClaudeExtraArgs),
		agent.NewCursorAdapter(cfg.CursorBin, cfg.CursorExtraArgs),
		agent.NewCustomAdapter(cfg.CustomName, cfg.CustomBin, cfg.CustomArgs),
		agent.NewMockAdapter(),
	)
	mgr := task.NewManager(cfg.MaxTasks)

	auditLog, err := audit.New(cfg.AuditLog)
	if err != nil {
		// A broken audit sink must not take the whole server down; degrade to
		// no audit and keep serving.
		log.Printf("warning: audit log %q disabled: %v", cfg.AuditLog, err)
		auditLog = &audit.Logger{}
	}
	defer auditLog.Close()
	mgr.SetAudit(auditLog)
	mgr.SetTaskTimeout(cfg.TaskTimeout)
	if auditLog.Enabled() {
		log.Printf("audit log: %s", cfg.AuditLog)
	}
	if cfg.TaskTimeout > 0 {
		log.Printf("task timeout: %s", cfg.TaskTimeout)
	}

	if !gate.Allowed() {
		// A rejected launch is the one event in this log that means something
		// tried to use the server and could not. Record who it was: it is the
		// only trace that a local process attempted it.
		auditLog.Log("pairing_rejected", map[string]any{
			"status": pairing.StatusName(gate.Status),
			"label":  gate.Label,
			"parent": launcher,
			"detail": gate.Detail,
		})
	}

	// The task registry has to outlive the process. Clients do start a second
	// server instance — sometimes alongside the first rather than in place of
	// it — and an instance that came up with an empty map used to report that
	// emptiness as fact while the other one's workers kept running.
	//
	// An unauthorized instance stays out of all of it. It will run no tasks, so
	// it has nothing to persist, and taking the lock would only make the real
	// server report a phantom rival it cannot see.
	if !gate.Allowed() {
		log.Printf("task state: not touched while locked")
	} else if store, err := state.Open(stateDir); err != nil {
		log.Printf("warning: task state is not being saved: %v", err)
	} else {
		defer store.Close()
		mgr.SetStore(store)
		store.Prune(cfg.MaxTasks)

		if prev, err := store.Acquire(); err != nil {
			log.Printf("warning: could not take the instance lock: %v", err)
		} else if prev != nil {
			instanceWarning = fmt.Sprintf(
				"another cli-agent-mcp server (pid %d, started %s) is still running and owns tasks this instance cannot watch or cancel. "+
					"Tasks it started appear here as %q once they are restored from disk. "+
					"If that process is no longer wanted, stop it — but note that doing so kills any worker still running under it.",
				prev.PID, prev.Started.Format(time.RFC3339), task.StatusOrphaned)
			log.Printf("warning: %s", instanceWarning)
		}

		if n := mgr.Restore(reg); n > 0 {
			log.Printf("restored %d task(s) from %s", n, store.Dir())
		}
		log.Printf("task state: %s", store.Dir())
	}

	if len(os.Args) > 1 && os.Args[1] == "--list-agents" {
		for _, a := range reg.All() {
			ok, detail := a.Available()
			status := "available"
			if !ok {
				status = "unavailable"
			}
			fmt.Printf("%-8s %-12s %s\n", a.Name(), status, detail)
		}
		return
	}

	// Advertise the MCP Apps extension so hosts that support interactive views
	// know this server can render one, and fetch the board's ui:// resource.
	// Setting Capabilities at all replaces the SDK's default, so logging has to
	// be restated here to keep it.
	caps := &mcp.ServerCapabilities{Logging: &mcp.LoggingCapabilities{}}
	caps.AddExtension(ui.ExtensionName, ui.Capability())

	serverInstructions := instructions
	if _, clientMsg := pairing.Explain(gate); clientMsg != "" {
		serverInstructions = clientMsg
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "cli-agent-mcp",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: serverInstructions,
		Capabilities: caps,
	})

	registerTools(srv, reg, mgr, cfg)

	// The tools stay registered when locked, and the middleware answers for
	// them. Serving an empty tool list instead would tell the model nothing —
	// it would simply conclude the server has no capabilities and move on,
	// leaving the user to guess why the thing they configured does nothing.
	if !gate.Allowed() {
		_, clientMsg := pairing.Explain(gate)
		srv.AddReceivingMiddleware(lockdown(clientMsg))
	}

	log.Printf("starting (default agent=%s, max_tasks=%d)", cfg.DefaultAgent, cfg.MaxTasks)
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

const instructions = `This server delegates coding/ops tasks to a headless CLI agent (Claude Code, Cursor, or a custom-configured tool) running on the host machine. The worker inherits that machine's environment, so it can reach whatever the host can — including private networks and hosts behind an SSH agent. Treat the worker as an extension of yourself: delegate, watch it work, and report the result as if you did it.

PREFERRED (seamless, no polling):
- agent_run_task — delegate a task and WAIT; the tool streams live progress notifications while the agent works, then returns the final result inline. Use this by default.
- agent_run_followup — continue the same session with another instruction, same streaming behavior.

If a task needs to run shell commands (git, tests, ssh, etc.) and seems to stall, the headless worker is likely waiting for a tool approval no one can give. Pass allowed_tools to pre-approve them, e.g. allowed_tools: ["Bash(git *)","PowerShell","Edit"]. This only pre-approves; the server's deny policy still applies. (Do not ask for extra_args — it's disabled by design.)

IMPORTANT — you cannot stop the worker once it starts. Progress notifications are informational and arrive after each step has already run, so there is no way to veto an action mid-task. For anything risky, destructive, or hard to undo (deleting data, touching production, force-pushing, changing infrastructure), call agent_plan_task FIRST: it makes the agent propose its steps while executing nothing. Show that plan to the user, and only then call agent_run_followup on the returned task_id to execute it.

BACKGROUND MODE (a tracked task you watch):
1. agent_start_task — start the task, get a task_id (returns immediately).
2. agent_watch — by default (until="done") it blocks until the task finishes, streaming live progress the whole time. If the task is longer than the watch window it returns early with running=true and a next_since_line: the task was NOT interrupted and nothing failed. Call agent_watch again with that since_line and keep going until running is false. This is the way to follow a long task — a single call cannot cover it, because the client abandons a tool call that takes too long.
3. Only if you need to actively supervise and possibly interrupt: call agent_watch with until="change" (returns on each new chunk so you can judge it), and agent_cancel_task if it drifts (terminates the worker and the process tree it created; a process the worker deliberately detached via ShellExecute can outlive it). To redirect: cancel and start a corrected task, or agent_send_followup once finished.

SHOWING THE USER WHERE THINGS STAND:
- agent_task_board — opens a live panel listing every task with its status, elapsed time and output, which keeps refreshing on its own and can cancel a running task. Progress notifications only exist while a call is in flight, so a backgrounded task is invisible once agent_start_task returns; the board is what fixes that. Call it right after agent_start_task, and whenever the user asks how their tasks are going.
- Outside this conversation the user can watch the same tasks themselves, from a terminal: ` + "`cli-agent-mcp tasks`" + ` lists them; ` + "`cli-agent-mcp logs <id>`" + ` (or with no id, for a picker) follows one live; ` + "`cli-agent-mcp logs --all`" + ` follows every running task at once; ` + "`cli-agent-mcp ui`" + ` opens a local web viewer. Mention this if the board does not render in their host, or if they want to follow a task without keeping this chat open. Those viewers are read-only — cancelling still goes through agent_cancel_task.

Also: agent_task_status / agent_get_output (poll/read on demand), agent_list_tasks, agent_list_agents.

Honest limits: agent_run_task's progress notifications are informational and post-hoc — you cannot veto a step mid-call. The worker runs one turn to completion, so you cannot inject a prompt into a turn already in flight; you steer between turns (cancel + restart, or follow-up). Interruption is stopping the process, not undoing what already ran — so for destructive work, agent_plan_task first.`

// ---- tool input types ---------------------------------------------------

type startInput struct {
	Prompt       string   `json:"prompt" jsonschema:"The task or instruction to delegate to the CLI agent."`
	Agent        string   `json:"agent,omitempty" jsonschema:"Which agent to use (e.g. claude, cursor, custom, mock). Call agent_list_agents to see what this server has available. Defaults to the server's configured default."`
	Cwd          string   `json:"cwd,omitempty" jsonschema:"Absolute working directory for the agent. Defaults to the server's configured default."`
	Model        string   `json:"model,omitempty" jsonschema:"Optional model override passed to the agent."`
	AllowedTools []string `json:"allowed_tools,omitempty" jsonschema:"Tools to pre-approve for this run so the headless worker doesn't stall waiting for approval it can't get (Claude Code --allowedTools). Patterns supported, e.g. [\"Bash(git *)\",\"Edit\",\"PowerShell\"]. This only pre-approves; the server's deny policy still applies."`
	ExtraArgs    []string `json:"extra_args,omitempty" jsonschema:"Extra CLI flags appended verbatim to this run (disabled by default on the server)."`
}

type idInput struct {
	TaskID string `json:"task_id" jsonschema:"The task id returned by agent_start_task."`
}

type followupInput struct {
	TaskID       string   `json:"task_id" jsonschema:"The task id whose session to continue."`
	Prompt       string   `json:"prompt" jsonschema:"The next instruction for the agent, continuing the same session."`
	AllowedTools []string `json:"allowed_tools,omitempty" jsonschema:"Tools to pre-approve for this run (Claude Code --allowedTools). Only pre-approves; the server's deny policy still applies."`
	ExtraArgs    []string `json:"extra_args,omitempty" jsonschema:"Extra CLI flags appended verbatim to this run (disabled by default on the server)."`
}

type outputInput struct {
	TaskID    string `json:"task_id" jsonschema:"The task id to read output from."`
	SinceLine int    `json:"since_line,omitempty" jsonschema:"0-based line index to start from (for incremental reads)."`
	MaxLines  int    `json:"max_lines,omitempty" jsonschema:"Maximum number of lines to return. 0 means all."`
	Raw       bool   `json:"raw,omitempty" jsonschema:"Return the raw JSONL transcript instead of the compact, filtered view."`
}

type watchInput struct {
	TaskID         string `json:"task_id" jsonschema:"The backgrounded task id to watch."`
	Until          string `json:"until,omitempty" jsonschema:"When to return: \"done\" (default) blocks in a SINGLE call until the task finishes, streaming live progress the whole time; \"change\" returns as soon as there's new output so you can peek and possibly interrupt."`
	SinceLine      int    `json:"since_line,omitempty" jsonschema:"Line index to watch from. Pass the next_since_line returned by the previous call to get only new output."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"How long this call may block before returning. For until=\"change\" it is how long to wait for new output (default 25, max 55); for until=\"done\" it overrides the server's watch window. Leave unset unless you know the client's tool-call limit."`
	Raw            bool   `json:"raw,omitempty" jsonschema:"Return the raw JSONL transcript instead of the compact, filtered view."`
}

// ---- tool output types --------------------------------------------------

type outputResult struct {
	TaskID     string `json:"task_id"`
	Status     string `json:"status"`
	FromLine   int    `json:"from_line"`
	ToLine     int    `json:"to_line"`
	TotalLines int    `json:"total_lines"`
	Text       string `json:"text"`
}

type watchResult struct {
	TaskID       string `json:"task_id"`
	Status       string `json:"status"`
	Running      bool   `json:"running"`
	NewSinceLine int    `json:"next_since_line"`
	Text         string `json:"text"`
}

type listResult struct {
	Tasks   []task.Snapshot `json:"tasks"`
	Warning string          `json:"warning,omitempty"`
}

// newListResult pairs a task listing with the standing instance warning, if
// any, so no caller can read the list without also seeing that it may be
// incomplete.
func newListResult(tasks []task.Snapshot) listResult {
	return listResult{Tasks: tasks, Warning: instanceWarning}
}

// withWarning prefixes text output with the instance warning. Structured output
// carries it too, but the text is what a human actually reads.
func withWarning(body string) string {
	if instanceWarning == "" {
		return body
	}
	return "⚠ " + instanceWarning + "\n\n" + body
}

type agentInfo struct {
	Name             string `json:"name"`
	Available        bool   `json:"available"`
	Detail           string `json:"detail"`
	IsDefault        bool   `json:"is_default"`
	SupportsPlanOnly bool   `json:"supports_plan_only"`
}

type agentsResult struct {
	DefaultAgent string      `json:"default_agent"`
	Agents       []agentInfo `json:"agents"`
}

// ---- registration -------------------------------------------------------

func textResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// errResult reports a tool-level failure the way the MCP spec prescribes: a
// successful call whose result carries IsError plus the full explanation.
//
// Handlers must return it with a nil error. The SDK's typed-handler wrapper
// throws the *mcp.CallToolResult away whenever the handler also returns a
// non-nil error, replacing it with err.Error() alone — so the rich message
// (which agent is unavailable, why extra_args was refused, what to set) would
// never reach the client.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "error: " + msg}},
	}
}

// ptr returns a pointer to v, for the SDK's optional *bool annotation fields.
func ptr[T any](v T) *T { return &v }

// lockdown turns an unauthorized server into one that answers every request
// with the same explanation instead of doing any work.
//
// It sits in front of the dispatcher rather than inside each handler so that a
// tool added later cannot forget to check. initialize, tools/list and ping are
// deliberately left alone: the client has to complete a handshake and read the
// tool list before it can call anything, and that is the path by which the
// explanation reaches the model at all.
func lockdown(msg string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "tools/call":
				// A tool-level error, not a protocol one: this is a refusal the
				// model is meant to read and relay, not a transport fault.
				return errResult(msg), nil
			case "resources/read":
				// The task board reads its data through here, and rendering it
				// empty would look like "no tasks" rather than "not authorized".
				return nil, errors.New(msg)
			}
			return next(ctx, method, req)
		}
	}
}

// readOnlyTool / mutatingTool are the annotation hints shared by the tools of
// each kind. Read-only tools only inspect this server's own task state;
// mutating ones launch or kill a worker that can touch anything the host can.
func readOnlyTool() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
}

func mutatingTool() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}
}

// checkExtraArgs enforces the extra_args policy. It fails closed: `extra_args`
// is appended after the operator's configured flags, so allowing it by default
// would let the calling model hand the agent e.g.
// --dangerously-skip-permissions and void the server's permission policy.
func checkExtraArgs(cfg config.Config, extra []string) error {
	if len(extra) > 0 && !cfg.AllowExtraArgs {
		return fmt.Errorf("extra_args is disabled on this server: refusing to pass %v to the agent, "+
			"because arbitrary flags can override the configured permission policy. "+
			"The operator can enable it with CLI_AGENT_MCP_ALLOW_EXTRA_ARGS=true", extra)
	}
	return nil
}

// resolveTarget picks the adapter and validated cwd for a task, returning a
// user-facing error message on failure.
func resolveTarget(reg *agent.Registry, cfg config.Config, agentName, cwd string) (agent.Adapter, string, string, error) {
	name := agentName
	if name == "" {
		name = cfg.DefaultAgent
	}
	a := reg.Get(name)
	if a == nil {
		return nil, "", fmt.Sprintf("unknown agent %q (available: %s)", name, strings.Join(reg.Names(), ", ")), fmt.Errorf("unknown agent %q", name)
	}
	if ok, detail := a.Available(); !ok {
		return nil, "", fmt.Sprintf("agent %q is not available: %s", name, detail), fmt.Errorf("agent %q unavailable", name)
	}
	resolved, err := task.ResolveCwd(cwd, cfg.DefaultCwd, cfg.AllowedCwds)
	if err != nil {
		return nil, "", err.Error(), err
	}
	return a, resolved, "", nil
}

// newProgressSink builds an EventSink that forwards each agent event to the MCP
// client as a progress notification, so the user watches the worker live. It
// returns nil when the client did not supply a progress token.
//
// The returned sink is safe for concurrent use: the task manager drives it from
// both the stdout and stderr pumps.
func newProgressSink(ctx context.Context, req *mcp.CallToolRequest) task.EventSink {
	token := req.Params.GetProgressToken()
	if token == nil {
		return nil
	}
	var (
		mu  sync.Mutex
		seq float64 // MCP requires progress to strictly increase
	)
	return func(ev agent.Event) {
		msg := ev.Text
		if ev.Final {
			if ev.FinalText != "" {
				msg = "✓ " + firstLine(ev.FinalText)
			} else {
				msg = "✓ done"
			}
		}
		if strings.TrimSpace(msg) == "" {
			return
		}
		mu.Lock()
		seq++
		n := seq
		mu.Unlock()
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Message:       truncate(msg, 500),
			Progress:      n,
		})
	}
}

// outcomeDetail surfaces the diagnostic fields the result text alone hides: the
// process exit code and the run error. Without them a run that produced no
// output reports only "status=failed" and the actual cause stays invisible.
// Returns "" when there is nothing to add.
func outcomeDetail(snap task.Snapshot) string {
	var parts []string
	if snap.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("exit_code=%d", *snap.ExitCode))
	}
	if snap.Error != "" {
		parts = append(parts, "error: "+snap.Error)
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, "; ") + "."
}

// finishText builds the inline text returned when a streaming run completes.
func finishText(snap task.Snapshot) string {
	body := snap.Result
	if body == "" {
		body = fmt.Sprintf("(agent produced no textual result; status=%s)", snap.Status)
	}
	header := fmt.Sprintf("Task %s finished with status %q.", snap.ID, snap.Status)
	if snap.IsError || snap.Status == task.StatusFailed {
		header = fmt.Sprintf("Task %s FAILED (status %q).", snap.ID, snap.Status)
	}
	return header + outcomeDetail(snap) + "\n\n" + body
}

// planText frames the outcome of a plan-only run, making it unmistakable that
// nothing ran and stating how to proceed.
func planText(snap task.Snapshot) string {
	body := snap.Result
	if body == "" {
		body = fmt.Sprintf("(agent produced no plan text; status=%s)", snap.Status)
	}
	if snap.IsError || snap.Status == task.StatusFailed {
		return fmt.Sprintf("Planning task %s FAILED (status %q).%s Nothing was executed.\n\n%s",
			snap.ID, snap.Status, outcomeDetail(snap), body)
	}
	return fmt.Sprintf("Task %s planned (status %q) — NOTHING WAS EXECUTED.%s\n\n%s\n\nReview this with the user. To carry it out, call agent_run_followup with task_id=%s.",
		snap.ID, snap.Status, outcomeDetail(snap), body, snap.ID)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func registerTools(srv *mcp.Server, reg *agent.Registry, mgr *task.Manager, cfg config.Config) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_run_task",
		Description: "Delegate a task to a local headless CLI agent (Claude Code or Cursor) and WAIT for it to finish, streaming live progress notifications as the agent works. This is the seamless, in-line mode — no polling — so it feels like you did the work yourself. Use agent_start_task instead only when you want fire-and-forget or several tasks running in parallel.",
		Annotations: mutatingTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in startInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return errResult("prompt is required"), task.Snapshot{}, nil
		}
		if err := checkExtraArgs(cfg, in.ExtraArgs); err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		a, cwd, emsg, err := resolveTarget(reg, cfg, in.Agent, in.Cwd)
		if err != nil {
			return errResult(emsg), task.Snapshot{}, nil
		}
		t, err := mgr.RunTaskStreaming(ctx, a, cwd, agent.RunSpec{
			Prompt:       in.Prompt,
			Model:        in.Model,
			AllowedTools: in.AllowedTools,
			ExtraArgs:    in.ExtraArgs,
		}, newProgressSink(ctx, req))
		if err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		snap := t.Snapshot()
		return textResult(finishText(snap)), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "agent_plan_task",
		Description: "Ask the agent to PLAN a task WITHOUT executing it: it inspects the code and proposes the steps it would take, but changes nothing and runs no commands. " +
			"Use this first for anything risky or destructive, show the plan to the user, then call agent_run_followup with the returned task_id to actually carry it out. " +
			"Fails if the selected agent cannot guarantee plan-only execution.",
		// Plan-only changes nothing on disk, but it still spawns a worker and
		// creates a tracked task, so it is not a read-only call.
		Annotations: mutatingTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in startInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return errResult("prompt is required"), task.Snapshot{}, nil
		}
		if err := checkExtraArgs(cfg, in.ExtraArgs); err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		a, cwd, emsg, err := resolveTarget(reg, cfg, in.Agent, in.Cwd)
		if err != nil {
			return errResult(emsg), task.Snapshot{}, nil
		}
		// Fail closed: never fall back to executing when plan-only was requested.
		if !agent.CanPlan(a) {
			msg := fmt.Sprintf("agent %q cannot guarantee plan-only execution, so refusing to run: it would carry the task out instead of planning it", a.Name())
			return errResult(msg), task.Snapshot{}, nil
		}
		t, err := mgr.RunTaskStreaming(ctx, a, cwd, agent.RunSpec{
			Prompt:       in.Prompt,
			Model:        in.Model,
			AllowedTools: in.AllowedTools,
			ExtraArgs:    in.ExtraArgs,
			PlanOnly:     true,
		}, newProgressSink(ctx, req))
		if err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		snap := t.Snapshot()
		return textResult(planText(snap)), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_run_followup",
		Description: "Continue a finished task's session with a new instruction and WAIT for it to finish, streaming live progress (same seamless mode as agent_run_task). Resumes the same agent conversation.",
		Annotations: mutatingTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in followupInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return errResult("prompt is required"), task.Snapshot{}, nil
		}
		if err := checkExtraArgs(cfg, in.ExtraArgs); err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		t, err := mgr.FollowupStreaming(ctx, in.TaskID, in.Prompt, in.AllowedTools, in.ExtraArgs, newProgressSink(ctx, req))
		if err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		snap := t.Snapshot()
		return textResult(finishText(snap)), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_start_task",
		Description: "Delegate a task to a local headless CLI agent (Claude Code or Cursor) WITHOUT waiting. Returns a task_id immediately; runs in the background. Use for fire-and-forget or running several agents in parallel, then poll agent_task_status. For the seamless single-task experience, prefer agent_run_task.",
		Annotations: mutatingTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in startInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return errResult("prompt is required"), task.Snapshot{}, nil
		}
		if err := checkExtraArgs(cfg, in.ExtraArgs); err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		a, cwd, emsg, err := resolveTarget(reg, cfg, in.Agent, in.Cwd)
		if err != nil {
			return errResult(emsg), task.Snapshot{}, nil
		}
		t, err := mgr.StartTask(a, cwd, agent.RunSpec{
			Prompt:       in.Prompt,
			Model:        in.Model,
			AllowedTools: in.AllowedTools,
			ExtraArgs:    in.ExtraArgs,
		})
		if err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		snap := t.Snapshot()
		msg := fmt.Sprintf("Started task %s on agent %q in %s. Call agent_task_board to show the user a live panel of it, or agent_watch to block until it finishes.", snap.ID, snap.Agent, snap.Cwd)
		return textResult(msg), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_task_status",
		Description: "Get the current status and result of a delegated task. Poll this until status is \"done\" or \"failed\".",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, task.Snapshot, error) {
		t, ok := mgr.Get(in.TaskID)
		if !ok {
			return errResult("unknown task " + in.TaskID), task.Snapshot{}, nil
		}
		snap := t.Snapshot()
		msg := fmt.Sprintf("Task %s: status=%s", snap.ID, snap.Status)
		if snap.Result != "" {
			msg += "\nResult: " + snap.Result
		}
		return textResult(msg), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_get_output",
		Description: "Fetch the streamed transcript of a task (raw agent output lines). Use since_line/max_lines for incremental reads on long tasks.",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in outputInput) (*mcp.CallToolResult, outputResult, error) {
		t, ok := mgr.Get(in.TaskID)
		if !ok {
			return errResult("unknown task " + in.TaskID), outputResult{}, nil
		}
		from, to, total, text := t.Output(in.SinceLine, in.MaxLines, cfg.Compact && !in.Raw)
		snap := t.Snapshot()
		res := outputResult{
			TaskID:     snap.ID,
			Status:     string(snap.Status),
			FromLine:   from,
			ToLine:     to,
			TotalLines: total,
			Text:       text,
		}
		body := text
		if body == "" {
			body = "(no output yet)"
		}
		return textResult(body), res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "agent_watch",
		Description: "Watch a backgrounded task (from agent_start_task), streaming its live progress to the user. " +
			"By default (until=\"done\") it blocks until the task finishes, streaming the whole time. If the task outlives the watch window it returns early with running=true and a next_since_line — that is NOT a failure and the task keeps going: call agent_watch again with that since_line, and keep repeating until running is false. " +
			"Use until=\"change\" instead when you want to actively supervise: it returns as soon as there's new output so you can judge it and call agent_cancel_task if it's going wrong (then loop, passing back next_since_line). " +
			"To steer, cancel and start a corrected task, or agent_send_followup once it has finished.",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in watchInput) (*mcp.CallToolResult, watchResult, error) {
		t, ok := mgr.Get(in.TaskID)
		if !ok {
			return errResult("unknown task " + in.TaskID), watchResult{}, nil
		}
		compact := cfg.Compact && !in.Raw

		// Stream each new line to the client as a progress notification, so a
		// single long-blocking watch still shows real-time activity.
		token := req.Params.GetProgressToken()
		var seq float64
		emit := func(msg string) {
			msg = strings.TrimSpace(msg)
			if token == nil || msg == "" {
				return
			}
			seq++
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token, Message: truncate(msg, 500), Progress: seq,
			})
		}
		emitChunk := func(text string) {
			for _, line := range strings.Split(text, "\n") {
				emit(line)
			}
		}

		// until="change": return on the first new output (interruptible supervision).
		if strings.EqualFold(in.Until, "change") {
			timeout := in.TimeoutSeconds
			if timeout <= 0 {
				timeout = 25
			}
			if timeout > 55 {
				timeout = 55
			}
			text, newSince, _, status, running := t.WatchFrom(ctx, in.SinceLine, time.Duration(timeout)*time.Second, compact)
			emitChunk(text)
			body := text
			if body == "" {
				if running {
					body = fmt.Sprintf("(no new output yet; still running — watch again from since_line=%d, or just use until=\"done\")", newSince)
				} else {
					body = fmt.Sprintf("(task %s; no more output)", status)
				}
			}
			return textResult(body), watchResult{TaskID: t.ID, Status: string(status), Running: running, NewSinceLine: newSince, Text: text}, nil
		}

		// until="done" (default): block until the task finishes OR the watch
		// window closes, whichever comes first.
		//
		// Blocking indefinitely looked right and was not: clients cap how long
		// they will wait on a tool call — Claude Desktop at 60s — and when that
		// cap hits, the call is abandoned and everything this handler was about
		// to return is discarded. The user sees the watch "cut off" with no
		// result at all, which is strictly worse than never having watched.
		// Returning first, carrying the output so far and the line to resume
		// from, turns that dead end into a call the model can simply repeat.
		window := cfg.WatchWindow
		if in.TimeoutSeconds > 0 {
			window = time.Duration(in.TimeoutSeconds) * time.Second
		}
		deadline := time.Now().Add(window)

		since := in.SinceLine
		start := time.Now()
		var status task.Status
		running := true
		for {
			slice := 10 * time.Second
			if left := time.Until(deadline); left < slice {
				slice = left
			}
			if slice <= 0 {
				break
			}
			text, newSince, _, st, run := t.WatchFrom(ctx, since, slice, compact)
			status, running, since = st, run, newSince
			if text != "" {
				emitChunk(text)
			} else if run {
				emit(fmt.Sprintf("… still running (%s)", time.Since(start).Round(time.Second)))
			}
			if !run || ctx.Err() != nil {
				break
			}
		}

		if running && ctx.Err() == nil {
			body := fmt.Sprintf(
				"Task %s is STILL RUNNING after %s. This is not a failure and the task was not interrupted.\n\n"+
					"Call agent_watch again with task_id=%s and since_line=%d to keep following it, and repeat until running is false.",
				t.ID, time.Since(start).Round(time.Second), t.ID, since)
			return textResult(body),
				watchResult{TaskID: t.ID, Status: string(status), Running: true, NewSinceLine: since}, nil
		}

		snap := t.Snapshot()
		final := snap.Result
		if final == "" {
			// Without the exit code and error, a watcher that ends on a failure
			// is told only that there was no text — the cause stays invisible.
			final = fmt.Sprintf("(no textual result; status=%s)%s", status, outcomeDetail(snap))
		}
		return textResult(fmt.Sprintf("Task %s finished with status %q.\n\n%s", snap.ID, status, final)),
			watchResult{TaskID: snap.ID, Status: string(status), Running: running, NewSinceLine: since, Text: final}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_send_followup",
		Description: "Continue a finished task's session with a new instruction (resumes the same agent conversation). Fails if the task is still running.",
		Annotations: mutatingTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in followupInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return errResult("prompt is required"), task.Snapshot{}, nil
		}
		if err := checkExtraArgs(cfg, in.ExtraArgs); err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		t, err := mgr.Followup(in.TaskID, in.Prompt, in.AllowedTools, in.ExtraArgs)
		if err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		snap := t.Snapshot()
		return textResult(fmt.Sprintf("Resumed task %s (session %s). Poll agent_task_status.", snap.ID, snap.SessionID)), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_cancel_task",
		Description: "Request cancellation of a running task (terminates the agent process).",
		// Cancelling an already-cancelled task changes nothing further.
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: ptr(true),
			IdempotentHint:  true,
			OpenWorldHint:   ptr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, task.Snapshot, error) {
		snap, err := mgr.Cancel(in.TaskID)
		if err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		return textResult(fmt.Sprintf("Task %s: status=%s", snap.ID, snap.Status)), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_list_tasks",
		Description: "List all known tasks (newest first) with their status.",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listResult, error) {
		tasks := mgr.List()
		return textResult(withWarning(fmt.Sprintf("%d task(s).", len(tasks)))), newListResult(tasks), nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_list_agents",
		Description: "List the CLI agents this server can drive and whether each is available on this machine.",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, agentsResult, error) {
		var infos []agentInfo
		for _, a := range reg.All() {
			ok, detail := a.Available()
			infos = append(infos, agentInfo{
				Name:             a.Name(),
				Available:        ok,
				Detail:           detail,
				IsDefault:        a.Name() == cfg.DefaultAgent,
				SupportsPlanOnly: agent.CanPlan(a),
			})
		}
		return textResult(fmt.Sprintf("Default agent: %s", cfg.DefaultAgent)),
			agentsResult{DefaultAgent: cfg.DefaultAgent, Agents: infos}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_diagnose",
		Description: "Diagnose the execution chain: the process's package identity (MSIX on Windows), whether spawning child processes works, and how each agent resolves its binary. Use when an agent fails with no explanation, or with an exit code but no output.",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, agent.DiagnosticReport, error) {
		rep := agent.Diagnose(ctx, reg)
		return textResult(rep.Text()), rep, nil
	})

	registerBoard(srv, mgr)
}

// registerBoard wires up the interactive task board: the ui:// resource holding
// the view, plus the tool that opens it.
//
// This is what makes a backgrounded task visible. Progress notifications only
// exist while a tool call is in flight, so once agent_start_task returns there
// is nothing left to watch. The board keeps running in the host's iframe and
// calls agent_list_tasks / agent_get_output on its own schedule, so the user
// sees each task advance without the model having to hold a call open.
func registerBoard(srv *mcp.Server, mgr *task.Manager) {
	srv.AddResource(&mcp.Resource{
		URI:         ui.ResourceURI,
		Name:        "task-board",
		Description: "Interactive panel listing every delegated task with its live status and output.",
		MIMEType:    ui.MIMEType,
		Meta:        ui.ResourceMeta(),
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      ui.ResourceURI,
				MIMEType: ui.MIMEType,
				Text:     ui.BoardHTML,
			}},
		}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "agent_task_board",
		Description: "Open the live task board: an interactive panel listing every delegated task with its status, elapsed time and streamed output, which refreshes itself and can cancel a running task. " +
			"Use it when the user asks how their tasks are going, and right after agent_start_task so they can follow along instead of waiting blind. " +
			"On hosts without interactive views this returns the same listing as plain text, which is still worth reading out.",
		Annotations: readOnlyTool(),
		Meta:        ui.ToolMeta(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listResult, error) {
		tasks := mgr.List()
		return textResult(withWarning(boardText(tasks))), newListResult(tasks), nil
	})
}

// boardText renders the task list as text. Hosts that don't implement
// interactive views show this instead of the board, so it has to stand on its
// own rather than point at a panel that will never appear.
func boardText(tasks []task.Snapshot) string {
	if len(tasks) == 0 {
		return "No tasks yet."
	}
	running := 0
	for _, t := range tasks {
		if t.Status == task.StatusRunning {
			running++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d task(s), %d running.\n", len(tasks), running)
	for _, t := range tasks {
		prompt := "(no prompt)"
		if n := len(t.Prompts); n > 0 {
			prompt = firstLine(t.Prompts[n-1])
		}
		fmt.Fprintf(&b, "\n%s  %s  [%s]  %d line(s)%s\n  %s",
			t.ID, t.Status, t.Agent, t.TotalLines, outcomeDetail(t), truncate(prompt, 120))
	}
	return b.String()
}

func printHelp() {
	fmt.Print(helpText())
}

// helpText is the usage block, kept separate from printHelp so the unknown-flag
// path can write it to stderr instead of stdout.
func helpText() string {
	return `cli-agent-mcp ` + version + `

An MCP (stdio) server that lets an MCP client (e.g. Claude Desktop) drive a local
headless CLI coding agent — Claude Code, Cursor, or any tool you configure — as a
background worker, with live progress streaming.

Usage:
  cli-agent-mcp                 Run the MCP server over stdio (how an MCP client launches it).
  cli-agent-mcp pair            Authorize an MCP client to drive this server. Run it once
                                per client; --install edits the client's config for you.
                                Until you do, any local process can start this server and
                                delegate work to an agent that inherits your environment.
  cli-agent-mcp tasks           List delegated tasks and their status, then exit.
  cli-agent-mcp logs [TASK]     Follow a task's output live in the terminal. Without
                                TASK it shows a picker; --all follows every running
                                task at once.
  cli-agent-mcp ui              Serve a local web viewer of tasks and live logs.
  cli-agent-mcp --list-agents   Print detected agents and availability, then exit.
  cli-agent-mcp --version       Print version.
  cli-agent-mcp --help          This help.

Configuration (environment variables):
  CLI_AGENT_MCP_DEFAULT_AGENT      Default agent: claude|cursor|custom|mock (default: claude)
  CLI_AGENT_MCP_CLAUDE_BIN         Claude Code launcher                 (default: claude)
  CLI_AGENT_MCP_CURSOR_BIN         Cursor launcher fallback             (default: cursor-agent)
  CLI_AGENT_MCP_PERMISSION_MODE    Claude --permission-mode             (default: acceptEdits)
                                     acceptEdits|auto|bypassPermissions|manual|dontAsk|plan
  CLI_AGENT_MCP_CLAUDE_EXTRA_ARGS  Extra Claude flags (';'-separated)
  CLI_AGENT_MCP_CURSOR_EXTRA_ARGS  Extra Cursor flags (';'-separated)
  CLI_AGENT_MCP_APPEND_SYSTEM_PROMPT  Standing guidance added to every task's system
                                   prompt. e.g. tell the worker to use the full
                                   Windows OpenSSH path for internal SSH (the bare
                                   'ssh' can't reach the 1Password agent).
  CLI_AGENT_MCP_DEFAULT_CWD        Default working directory
  CLI_AGENT_MCP_ALLOWED_CWDS       Restrict task cwd to these roots (';'-separated)
  CLI_AGENT_MCP_MAX_TASKS          Max retained tasks                   (default: 100)
  CLI_AGENT_MCP_AUDIT_LOG          Path to a JSONL audit log of what the worker did
  CLI_AGENT_MCP_WATCH_WINDOW_SECONDS  How long one agent_watch call may block before
                                   returning a resumable partial. Keep it under the
                                   client's tool-call limit (Claude Desktop: 60s), or
                                   the call is abandoned and its result lost.
                                                                        (default: 50)
  CLI_AGENT_MCP_STATE_DIR          Where task records and the instance lock live, so
                                   tasks survive a restart and a second instance is
                                   detected instead of silently starting empty.
                                   (default: %AppData%\cli-agent-mcp, ~/.config/... )
  CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS  Kill a turn that runs longer than this — a
                                   safety net for a worker hung on a permission
                                   prompt.                              (default: 0 = off)
  CLI_AGENT_MCP_COMPACT            Return a filtered, readable transcript from
                                   agent_get_output/agent_watch instead of raw
                                   JSONL.                               (default: true)
  CLI_AGENT_MCP_TOKEN              The pairing credential the client presents at launch.
                                   Set it with 'cli-agent-mcp pair', in the client's own
                                   config — not by hand here. Once paired, a launcher
                                   without it gets a server that refuses every tool call.

Bounding what the worker may do:
  A headless run executes tool calls by default. To BLOCK dangerous ones use the
  denylist — the allowlist is additive (pre-approve only), NOT an exclusive gate.
  CLI_AGENT_MCP_DISALLOWED_TOOLS   Claude --disallowedTools — the real deny gate.
                                   e.g. "Bash(rm:*),Bash(git push:*),Bash(sudo:*)"
  CLI_AGENT_MCP_ALLOWED_TOOLS      Claude --allowedTools — pre-approves tools (does
                                   not restrict). e.g. "Read,Edit,Bash(git status)"
  CLI_AGENT_MCP_ALLOW_EXTRA_ARGS   Let callers pass arbitrary agent flags via the
                                   tool's extra_args   (default: false)
                                   Keep this OFF: the caller is a model, and extra
                                   flags can override the policy above.

Custom agent (drive any CLI without writing Go):
  CLI_AGENT_MCP_CUSTOM_BIN         The agent's executable
  CLI_AGENT_MCP_CUSTOM_ARGS        Arg template (';'-separated). Placeholders:
                                     {{prompt}} {{cwd}} {{model}} {{session}}
                                   An arg whose placeholder is empty is dropped, so
                                   prefer the '--flag=value' form for optional ones.
  CLI_AGENT_MCP_CUSTOM_NAME        Name to expose it as        (default: custom)

  Example:
    CLI_AGENT_MCP_CUSTOM_BIN=aider
    CLI_AGENT_MCP_CUSTOM_ARGS=--no-pretty;--yes;--message;{{prompt}}
`
}
