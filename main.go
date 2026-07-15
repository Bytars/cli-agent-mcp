// Command cli-agent-mcp is a Model Context Protocol server that lets an
// orchestrating client (e.g. Claude Desktop) drive a local headless CLI coding
// agent — Claude Code or Cursor — as a background worker.
//
// The worker runs on this machine and inherits this process's environment, so
// anything the machine can reach (a VPN, internal servers via the 1Password SSH
// agent) is transparently available to it. The orchestrator delegates a task,
// polls for completion, reads the result, and can send follow-up turns — no
// copy-pasting between two chat windows.
//
// Transport is stdio, matching how Claude Desktop launches github-mcp-server.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anher/cli-agent-mcp/internal/agent"
	"github.com/anher/cli-agent-mcp/internal/config"
	"github.com/anher/cli-agent-mcp/internal/task"
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
		agent.NewClaudeAdapter(cfg.ClaudeBin, cfg.PermissionMode, cfg.ClaudeExtraArgs),
		agent.NewCursorAdapter(cfg.CursorBin, cfg.CursorExtraArgs),
		agent.NewMockAdapter(),
	)
	mgr := task.NewManager(cfg.MaxTasks)

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

const instructions = `This server delegates coding/ops tasks to a local headless CLI agent (Claude Code or Cursor) that runs on the user's machine with full access to their VPN and SSH-agent-backed internal servers. Treat the worker as an extension of yourself: delegate, watch it work, and report the result as if you did it.

PREFERRED (seamless, no polling):
- agent_run_task — delegate a task and WAIT; the tool streams live progress notifications while the agent works, then returns the final result inline. Use this by default.
- agent_run_followup — continue the same session with another instruction, same streaming behavior.

ADVANCED (parallel / fire-and-forget):
- agent_start_task — start a task in the background, returns a task_id immediately. Use only to run multiple agents at once or to kick something off and check later.
- agent_task_status / agent_get_output — poll status and read the transcript of a backgrounded task.
- agent_send_followup — non-blocking follow-up on a backgrounded task.

Utility: agent_cancel_task, agent_list_tasks, agent_list_agents.

Long tasks may take minutes; agent_run_task keeps the connection alive via progress updates, so just let it run.`

// ---- tool input types ---------------------------------------------------

type startInput struct {
	Prompt    string   `json:"prompt" jsonschema:"The task or instruction to delegate to the CLI agent."`
	Agent     string   `json:"agent,omitempty" jsonschema:"Which agent to use: claude, cursor, or mock. Defaults to the server's configured default."`
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

// ---- tool output types --------------------------------------------------

type outputResult struct {
	TaskID     string `json:"task_id"`
	Status     string `json:"status"`
	FromLine   int    `json:"from_line"`
	ToLine     int    `json:"to_line"`
	TotalLines int    `json:"total_lines"`
	Text       string `json:"text"`
}

type listResult struct {
	Tasks []task.Snapshot `json:"tasks"`
}

type agentInfo struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail"`
	IsDefault bool   `json:"is_default"`
}

type agentsResult struct {
	DefaultAgent string      `json:"default_agent"`
	Agents       []agentInfo `json:"agents"`
}

// ---- registration -------------------------------------------------------

func textResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
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
func newProgressSink(ctx context.Context, req *mcp.CallToolRequest) task.EventSink {
	token := req.Params.GetProgressToken()
	if token == nil {
		return nil
	}
	var seq float64 // only the stdout pump goroutine calls the sink → no lock needed
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
		seq++
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Message:       truncate(msg, 500),
			Progress:      seq,
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
		Name:        "agent_run_followup",
		Description: "Continue a finished task's session with a new instruction and WAIT for it to finish, streaming live progress (same seamless mode as agent_run_task). Resumes the same agent conversation.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in followupInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return textResult("error: prompt is required"), task.Snapshot{}, fmt.Errorf("prompt is required")
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
		Name:        "agent_send_followup",
		Description: "Continue a finished task's session with a new instruction (resumes the same agent conversation). Fails if the task is still running.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in followupInput) (*mcp.CallToolResult, task.Snapshot, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return textResult("error: prompt is required"), task.Snapshot{}, fmt.Errorf("prompt is required")
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
				Name:      a.Name(),
				Available: ok,
				Detail:    detail,
				IsDefault: a.Name() == cfg.DefaultAgent,
			})
		}
		return textResult(fmt.Sprintf("Default agent: %s", cfg.DefaultAgent)),
			agentsResult{DefaultAgent: cfg.DefaultAgent, Agents: infos}, nil
	})
}

func printHelp() {
	fmt.Print(`cli-agent-mcp ` + version + `

An MCP (stdio) server that lets Claude Desktop drive a local headless CLI coding
agent (Claude Code or Cursor) as a background worker.

Usage:
  cli-agent-mcp                 Run the MCP server over stdio (how Claude Desktop launches it).
  cli-agent-mcp --list-agents   Print detected agents and availability, then exit.
  cli-agent-mcp --version       Print version.
  cli-agent-mcp --help          This help.

Configuration (environment variables):
  CLI_AGENT_MCP_DEFAULT_AGENT      Default agent: claude|cursor|mock  (default: claude)
  CLI_AGENT_MCP_CLAUDE_BIN         Claude Code launcher                (default: claude)
  CLI_AGENT_MCP_CURSOR_BIN         Cursor launcher fallback            (default: cursor-agent)
  CLI_AGENT_MCP_PERMISSION_MODE    Claude --permission-mode            (default: acceptEdits)
  CLI_AGENT_MCP_CLAUDE_EXTRA_ARGS  Extra Claude flags (';'-separated)
  CLI_AGENT_MCP_CURSOR_EXTRA_ARGS  Extra Cursor flags (';'-separated)
  CLI_AGENT_MCP_DEFAULT_CWD        Default working directory
  CLI_AGENT_MCP_ALLOWED_CWDS       Restrict task cwd to these roots (';'-separated)
  CLI_AGENT_MCP_MAX_TASKS          Max retained tasks                  (default: 100)
`)
}
