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
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andresh0816/cli-agent-mcp/internal/agent"
	"github.com/andresh0816/cli-agent-mcp/internal/audit"
	"github.com/andresh0816/cli-agent-mcp/internal/config"
	"github.com/andresh0816/cli-agent-mcp/internal/task"
)

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
		}
	}

	// Logs must go to stderr; stdout is the MCP wire.
	log.SetOutput(os.Stderr)
	log.SetPrefix("cli-agent-mcp: ")
	log.SetFlags(0)

	cfg := config.Load()

	reg := agent.NewRegistry(
		agent.NewClaudeAdapter(cfg.ClaudeBin, cfg.PermissionMode, cfg.AllowedTools, cfg.DisallowedTools, cfg.ClaudeExtraArgs),
		agent.NewCursorAdapter(cfg.CursorBin, cfg.CursorExtraArgs),
		agent.NewCustomAdapter(cfg.CustomName, cfg.CustomBin, cfg.CustomArgs),
		agent.NewMockAdapter(),
	)
	mgr := task.NewManager(cfg.MaxTasks)

	auditLog, err := audit.New(cfg.AuditLog)
	if err != nil {
		log.Fatalf("opening audit log %q: %v", cfg.AuditLog, err)
	}
	defer auditLog.Close()
	mgr.SetAudit(auditLog)
	if auditLog.Enabled() {
		log.Printf("audit log: %s", cfg.AuditLog)
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

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "cli-agent-mcp",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	registerTools(srv, reg, mgr, cfg)

	log.Printf("starting (default agent=%s, max_tasks=%d)", cfg.DefaultAgent, cfg.MaxTasks)
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

const instructions = `This server delegates coding/ops tasks to a headless CLI agent (Claude Code, Cursor, or a custom-configured tool) running on the host machine. The worker inherits that machine's environment, so it can reach whatever the host can — including private networks and hosts behind an SSH agent. Treat the worker as an extension of yourself: delegate, watch it work, and report the result as if you did it.

PREFERRED (seamless, no polling):
- agent_run_task — delegate a task and WAIT; the tool streams live progress notifications while the agent works, then returns the final result inline. Use this by default.
- agent_run_followup — continue the same session with another instruction, same streaming behavior.

IMPORTANT — you cannot stop the worker once it starts. Progress notifications are informational and arrive after each step has already run, so there is no way to veto an action mid-task. For anything risky, destructive, or hard to undo (deleting data, touching production, force-pushing, changing infrastructure), call agent_plan_task FIRST: it makes the agent propose its steps while executing nothing. Show that plan to the user, and only then call agent_run_followup on the returned task_id to execute it.

DIRECTOR MODE (you supervise and can interrupt):
When the user wants you actively watching — or the task is risky — run it in the background and steer it yourself:
1. agent_start_task — start the task, get a task_id (returns immediately).
2. agent_watch — blocks until new output arrives, returns what the worker just did. Between calls YOU are active and reasoning, so this is where you judge whether it's on track. Pass back next_since_line each call, and keep looping while running is true.
3. If it goes off the rails, agent_cancel_task stops it immediately (kills the whole process tree). To redirect, cancel and agent_start_task a corrected task, or agent_send_followup once it has finished.
This is the way to "watch it like the user would and stop it if it drifts." Prefer it over agent_run_task whenever supervision matters.

Also: agent_task_status / agent_get_output (poll/read on demand), agent_list_tasks, agent_list_agents.

Honest limits: agent_run_task's progress notifications are informational and post-hoc — you cannot veto a step mid-call. The worker runs one turn to completion, so you cannot inject a prompt into a turn already in flight; you steer between turns (cancel + restart, or follow-up). Interruption is stopping the process, not undoing what already ran — so for destructive work, agent_plan_task first.`

// ---- tool input types ---------------------------------------------------

type startInput struct {
	Prompt    string   `json:"prompt" jsonschema:"The task or instruction to delegate to the CLI agent."`
	Agent     string   `json:"agent,omitempty" jsonschema:"Which agent to use (e.g. claude, cursor, custom, mock). Call agent_list_agents to see what this server has available. Defaults to the server's configured default."`
	Cwd       string   `json:"cwd,omitempty" jsonschema:"Absolute working directory for the agent. Defaults to the server's configured default."`
	Model     string   `json:"model,omitempty" jsonschema:"Optional model override passed to the agent."`
	ExtraArgs []string `json:"extra_args,omitempty" jsonschema:"Extra CLI flags appended verbatim to this run."`
}

type idInput struct {
	TaskID string `json:"task_id" jsonschema:"The task id returned by agent_start_task."`
}

type followupInput struct {
	TaskID    string   `json:"task_id" jsonschema:"The task id whose session to continue."`
	Prompt    string   `json:"prompt" jsonschema:"The next instruction for the agent, continuing the same session."`
	ExtraArgs []string `json:"extra_args,omitempty" jsonschema:"Extra CLI flags appended verbatim to this run."`
}

type outputInput struct {
	TaskID    string `json:"task_id" jsonschema:"The task id to read output from."`
	SinceLine int    `json:"since_line,omitempty" jsonschema:"0-based line index to start from (for incremental reads)."`
	MaxLines  int    `json:"max_lines,omitempty" jsonschema:"Maximum number of lines to return. 0 means all."`
}

type watchInput struct {
	TaskID         string `json:"task_id" jsonschema:"The backgrounded task id to watch."`
	SinceLine      int    `json:"since_line,omitempty" jsonschema:"Line index to watch from. Pass the next_since_line returned by the previous call to get only new output."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"How long to wait for new output before returning (default 25, max 55)."`
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
	Tasks []task.Snapshot `json:"tasks"`
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
	return header + "\n\n" + body
}

// planText frames the outcome of a plan-only run, making it unmistakable that
// nothing ran and stating how to proceed.
func planText(snap task.Snapshot) string {
	body := snap.Result
	if body == "" {
		body = fmt.Sprintf("(agent produced no plan text; status=%s)", snap.Status)
	}
	if snap.IsError || snap.Status == task.StatusFailed {
		return fmt.Sprintf("Planning task %s FAILED (status %q). Nothing was executed.\n\n%s", snap.ID, snap.Status, body)
	}
	return fmt.Sprintf("Task %s planned — NOTHING WAS EXECUTED.\n\n%s\n\nReview this with the user. To carry it out, call agent_run_followup with task_id=%s.",
		snap.ID, body, snap.ID)
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
	}, func(ctx context.Context, req *mcp.CallToolRequest, in startInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return textResult("error: prompt is required"), task.Snapshot{}, fmt.Errorf("prompt is required")
		}
		if err := checkExtraArgs(cfg, in.ExtraArgs); err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		a, cwd, emsg, err := resolveTarget(reg, cfg, in.Agent, in.Cwd)
		if err != nil {
			return textResult("error: " + emsg), task.Snapshot{}, err
		}
		t, err := mgr.RunTaskStreaming(ctx, a, cwd, agent.RunSpec{
			Prompt:    in.Prompt,
			Model:     in.Model,
			ExtraArgs: in.ExtraArgs,
		}, newProgressSink(ctx, req))
		if err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		snap := t.Snapshot()
		return textResult(finishText(snap)), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "agent_plan_task",
		Description: "Ask the agent to PLAN a task WITHOUT executing it: it inspects the code and proposes the steps it would take, but changes nothing and runs no commands. " +
			"Use this first for anything risky or destructive, show the plan to the user, then call agent_run_followup with the returned task_id to actually carry it out. " +
			"Fails if the selected agent cannot guarantee plan-only execution.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in startInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return textResult("error: prompt is required"), task.Snapshot{}, fmt.Errorf("prompt is required")
		}
		if err := checkExtraArgs(cfg, in.ExtraArgs); err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		a, cwd, emsg, err := resolveTarget(reg, cfg, in.Agent, in.Cwd)
		if err != nil {
			return textResult("error: " + emsg), task.Snapshot{}, err
		}
		// Fail closed: never fall back to executing when plan-only was requested.
		if !agent.CanPlan(a) {
			msg := fmt.Sprintf("agent %q cannot guarantee plan-only execution, so refusing to run: it would carry the task out instead of planning it", a.Name())
			return textResult("error: " + msg), task.Snapshot{}, fmt.Errorf("%s", msg)
		}
		t, err := mgr.RunTaskStreaming(ctx, a, cwd, agent.RunSpec{
			Prompt:    in.Prompt,
			Model:     in.Model,
			ExtraArgs: in.ExtraArgs,
			PlanOnly:  true,
		}, newProgressSink(ctx, req))
		if err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		snap := t.Snapshot()
		return textResult(planText(snap)), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_run_followup",
		Description: "Continue a finished task's session with a new instruction and WAIT for it to finish, streaming live progress (same seamless mode as agent_run_task). Resumes the same agent conversation.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in followupInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return textResult("error: prompt is required"), task.Snapshot{}, fmt.Errorf("prompt is required")
		}
		if err := checkExtraArgs(cfg, in.ExtraArgs); err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		t, err := mgr.FollowupStreaming(ctx, in.TaskID, in.Prompt, in.ExtraArgs, newProgressSink(ctx, req))
		if err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		snap := t.Snapshot()
		return textResult(finishText(snap)), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_start_task",
		Description: "Delegate a task to a local headless CLI agent (Claude Code or Cursor) WITHOUT waiting. Returns a task_id immediately; runs in the background. Use for fire-and-forget or running several agents in parallel, then poll agent_task_status. For the seamless single-task experience, prefer agent_run_task.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in startInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return textResult("error: prompt is required"), task.Snapshot{}, fmt.Errorf("prompt is required")
		}
		if err := checkExtraArgs(cfg, in.ExtraArgs); err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		a, cwd, emsg, err := resolveTarget(reg, cfg, in.Agent, in.Cwd)
		if err != nil {
			return textResult("error: " + emsg), task.Snapshot{}, err
		}
		t, err := mgr.StartTask(a, cwd, agent.RunSpec{
			Prompt:    in.Prompt,
			Model:     in.Model,
			ExtraArgs: in.ExtraArgs,
		})
		if err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		snap := t.Snapshot()
		msg := fmt.Sprintf("Started task %s on agent %q in %s. Poll agent_task_status until status is \"done\" or \"failed\".", snap.ID, snap.Agent, snap.Cwd)
		return textResult(msg), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_task_status",
		Description: "Get the current status and result of a delegated task. Poll this until status is \"done\" or \"failed\".",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, task.Snapshot, error) {
		t, ok := mgr.Get(in.TaskID)
		if !ok {
			return textResult("error: unknown task " + in.TaskID), task.Snapshot{}, fmt.Errorf("unknown task %q", in.TaskID)
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
	}, func(ctx context.Context, req *mcp.CallToolRequest, in outputInput) (*mcp.CallToolResult, outputResult, error) {
		t, ok := mgr.Get(in.TaskID)
		if !ok {
			return textResult("error: unknown task " + in.TaskID), outputResult{}, fmt.Errorf("unknown task %q", in.TaskID)
		}
		from, to, total, text := t.Output(in.SinceLine, in.MaxLines)
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
		Description: "Supervise a backgrounded task in near-real-time. Blocks until new output arrives or the task ends, then returns the new transcript lines. " +
			"This is how you DIRECT a worker: start it with agent_start_task, then loop calling agent_watch (passing back next_since_line each time) to read what it's doing as it happens. " +
			"If you see it going off the rails, call agent_cancel_task to stop it immediately; to steer, cancel and start a corrected task, or agent_send_followup once it finishes. " +
			"Keep looping while running is true.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in watchInput) (*mcp.CallToolResult, watchResult, error) {
		t, ok := mgr.Get(in.TaskID)
		if !ok {
			return textResult("error: unknown task " + in.TaskID), watchResult{}, fmt.Errorf("unknown task %q", in.TaskID)
		}
		timeout := in.TimeoutSeconds
		if timeout <= 0 {
			timeout = 25
		}
		if timeout > 55 {
			timeout = 55
		}
		lines, newSince, _, status, running := t.WatchFrom(ctx, in.SinceLine, time.Duration(timeout)*time.Second)
		text := strings.Join(lines, "\n")
		res := watchResult{
			TaskID:       t.ID,
			Status:       string(status),
			Running:      running,
			NewSinceLine: newSince,
			Text:         text,
		}
		body := text
		if body == "" {
			if running {
				body = fmt.Sprintf("(no new output yet; task still running — call agent_watch again with since_line=%d)", newSince)
			} else {
				body = fmt.Sprintf("(task %s; no more output)", status)
			}
		}
		return textResult(body), res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_send_followup",
		Description: "Continue a finished task's session with a new instruction (resumes the same agent conversation). Fails if the task is still running.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in followupInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return textResult("error: prompt is required"), task.Snapshot{}, fmt.Errorf("prompt is required")
		}
		if err := checkExtraArgs(cfg, in.ExtraArgs); err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		t, err := mgr.Followup(in.TaskID, in.Prompt, in.ExtraArgs)
		if err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		snap := t.Snapshot()
		return textResult(fmt.Sprintf("Resumed task %s (session %s). Poll agent_task_status.", snap.ID, snap.SessionID)), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_cancel_task",
		Description: "Request cancellation of a running task (terminates the agent process).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in idInput) (*mcp.CallToolResult, task.Snapshot, error) {
		snap, err := mgr.Cancel(in.TaskID)
		if err != nil {
			return textResult("error: " + err.Error()), task.Snapshot{}, err
		}
		return textResult(fmt.Sprintf("Task %s: status=%s", snap.ID, snap.Status)), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_list_tasks",
		Description: "List all known tasks (newest first) with their status.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listResult, error) {
		tasks := mgr.List()
		return textResult(fmt.Sprintf("%d task(s).", len(tasks))), listResult{Tasks: tasks}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "agent_list_agents",
		Description: "List the CLI agents this server can drive and whether each is available on this machine.",
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
}

func printHelp() {
	fmt.Print(`cli-agent-mcp ` + version + `

An MCP (stdio) server that lets an MCP client (e.g. Claude Desktop) drive a local
headless CLI coding agent — Claude Code, Cursor, or any tool you configure — as a
background worker, with live progress streaming.

Usage:
  cli-agent-mcp                 Run the MCP server over stdio (how an MCP client launches it).
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
  CLI_AGENT_MCP_DEFAULT_CWD        Default working directory
  CLI_AGENT_MCP_ALLOWED_CWDS       Restrict task cwd to these roots (';'-separated)
  CLI_AGENT_MCP_MAX_TASKS          Max retained tasks                   (default: 100)
  CLI_AGENT_MCP_AUDIT_LOG          Path to a JSONL audit log of what the worker did

Bounding what the worker may do (recommended over a permissive mode):
  CLI_AGENT_MCP_ALLOWED_TOOLS      Claude --allowedTools allowlist, patterns supported.
                                   e.g. "Bash(git *),Bash(npm test),Edit,Read"
  CLI_AGENT_MCP_DISALLOWED_TOOLS   Claude --disallowedTools denylist.
  CLI_AGENT_MCP_ALLOW_EXTRA_ARGS   Let callers pass arbitrary agent flags via the
                                   tool's extra_args   (default: false)
                                   Keep this OFF: the caller is a model, and extra
                                   flags can override the permission policy above.

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
`)
}
