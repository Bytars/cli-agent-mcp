// SPDX-License-Identifier: Apache-2.0

// Package config holds runtime configuration for the CLI-agent MCP server.
//
// All settings are read from environment variables so the server can be
// configured entirely from a Claude Desktop `mcpServers` entry, exactly like
// github-mcp-server. Every value has a sensible default.
package config

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// warnInvalid reports a misconfigured environment variable. It goes through the
// standard logger, which main points at stderr — never stdout, which is the MCP
// wire. A bad value is never fatal: the default is applied and the server runs.
func warnInvalid(key, value, reason string, def any) {
	log.Printf("warning: %s=%q %s; using default %v", key, value, reason, def)
}

// Config is the fully-resolved server configuration.
type Config struct {
	// DefaultAgent is used when a tool call does not specify one.
	DefaultAgent string

	// ClaudeBin is the command (name in PATH or absolute path) used to launch
	// Claude Code. Defaults to "claude".
	ClaudeBin string

	// CursorBin is the command used to launch Cursor's headless agent when
	// auto-detection fails. Defaults to "cursor-agent".
	CursorBin string

	// PermissionMode is passed to Claude Code's --permission-mode. Because the
	// agent runs headless (no human at the terminal to approve prompts), this
	// governs how autonomous it is. See README for the safety trade-offs.
	PermissionMode string

	// AllowedTools / DisallowedTools are passed to Claude Code's --allowedTools
	// and --disallowedTools (patterns supported, e.g. "Bash(git push:*),Edit").
	//
	// Important, verified against Claude Code 2.1.207: a headless `-p` run
	// EXECUTES tool calls by default, and --allowedTools is ADDITIVE (it only
	// pre-approves / removes prompts) — it does NOT act as an exclusive allowlist.
	// The reliable restriction is DisallowedTools, which hard-denies matching
	// tools/commands. This is server-side policy tool callers cannot override.
	AllowedTools    string
	DisallowedTools string

	// AllowExtraArgs controls whether tool calls may append arbitrary CLI flags
	// to the agent through the `extra_args` parameter.
	//
	// It defaults to FALSE, and that default is a security boundary: the client
	// driving this server is itself a model, and `extra_args` is appended after
	// the flags configured here. Left open, a caller could pass
	// --dangerously-skip-permissions (or its own --allowedTools) and silently
	// void the policy the operator configured. Only enable it when you trust the
	// caller as much as you trust your own shell.
	AllowExtraArgs bool

	// ClaudeExtraArgs / CursorExtraArgs are appended verbatim to every launch,
	// letting the user tune flags without a rebuild.
	ClaudeExtraArgs []string
	CursorExtraArgs []string

	// AppendSystemPrompt is added to Claude Code's system prompt on every task
	// (--append-system-prompt). Use it for standing, machine-specific guidance —
	// e.g. "for SSH to internal servers use the full Windows OpenSSH path, the
	// bare ssh can't reach the 1Password agent".
	AppendSystemPrompt string

	// CustomName / CustomBin / CustomArgs configure the generic adapter, which
	// can drive any CLI agent without writing Go. CustomArgs is a template whose
	// entries may contain {{prompt}}, {{cwd}}, {{model}} and {{session}}.
	CustomName string
	CustomBin  string
	CustomArgs []string

	// DefaultCwd is the working directory used when a tool call omits `cwd`.
	// Empty means "the server process's own working directory".
	DefaultCwd string

	// AllowedCwds, when non-empty, restricts every task's working directory to
	// live under one of these roots. A safety guardrail against a delegating
	// model wandering the filesystem.
	AllowedCwds []string

	// MaxTasks caps the number of tasks retained in memory.
	MaxTasks int

	// WorktreeDir is where isolated task checkouts are created. Empty puts them
	// alongside the task records, under StateDir — anywhere but inside the
	// repository, which would make them show up as untracked clutter in the very
	// diff they exist to produce.
	WorktreeDir string

	// MaxCostUSD bounds what one task may spend, in US dollars. Zero disables it.
	//
	// It is enforced in two places because neither alone is enough. Claude Code
	// is given the figure as --max-budget-usd, which it applies itself and can
	// act on mid-turn — that is the real protection. But that flag is per
	// invocation, so a task driven through ten follow-ups would get the whole
	// budget ten times over; the server therefore also tracks what a task has
	// spent across all its turns and refuses to start another once it is over.
	MaxCostUSD float64

	// MaxConcurrent caps how many workers may run at the same time. It is a
	// different limit from MaxTasks, which only bounds retained records: a
	// headless coding agent is a heavyweight process, so an orchestrator that
	// fans out ten tasks can exhaust the machine while every other limit still
	// looks satisfied. Zero means no limit.
	MaxConcurrent int

	// AuditLog is a file path for a JSONL audit trail of everything the worker
	// was asked to do. Empty disables it.
	AuditLog string

	// WatchWindow bounds how long a single agent_watch call blocks before
	// returning a resumable partial result. It exists because clients cap tool
	// calls: blocking past that cap loses the response entirely, so the server
	// returns first and tells the caller to come back.
	WatchWindow time.Duration

	// StateDir is where the task registry and the instance lock live, so a
	// restarted or second server instance can still see earlier tasks. Empty
	// means the per-user default.
	StateDir string

	// TaskTimeout, if > 0, cancels any turn that runs longer than this. It is a
	// safety net against a worker that hangs — e.g. blocked on a permission
	// prompt with no human to approve it. Zero means no timeout.
	TaskTimeout time.Duration

	// AskPermission lets a worker put a permission request to the person who
	// delegated the task, instead of stalling on a prompt nobody can answer.
	//
	// It is on by default and reaches every client. One that declared the
	// elicitation capability is asked directly; for the rest the request is
	// parked on the task and released by agent_answer_permission. Turning it
	// off restores the older behaviour, where a tool that is neither
	// pre-approved nor denied stalls until the task timeout.
	AskPermission bool

	// PermissionTimeout bounds how long a worker waits for that answer. It has
	// to be generous — there is a human at the other end who may be looking at
	// something else — but finite, or an unattended run waits forever.
	PermissionTimeout time.Duration

	// Compact controls whether agent_get_output / agent_watch return a filtered,
	// human-readable transcript by default (dropping the noisy init/config dump)
	// rather than raw JSONL.
	Compact bool

	// Token is the pairing credential the launching client presented, proving it
	// is one this server was configured for. It is set by `cli-agent-mcp pair`
	// in the client's own config, never by hand here. Empty means none was
	// offered, which only matters once the server has been paired.
	//
	// It authorizes the launch. It does not protect the MCP conversation, which
	// runs over a private pipe with nothing on it to intercept — see
	// internal/pairing for what this does and does not buy.
	Token string
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// getbool parses a permissive boolean env value, falling back to def. An
// unrecognized value is reported on stderr rather than swallowed, so a typo
// like CLI_AGENT_MCP_ALLOW_EXTRA_ARGS=tru doesn't silently mean something else.
func getbool(key string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	switch strings.ToLower(raw) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		warnInvalid(key, raw, "is not a recognized boolean (1/true/yes/on, 0/false/no/off)", def)
		return def
	}
}

// getPositiveInt reads an integer env value that must be greater than zero,
// falling back to def and reporting anything unusable rather than silently
// accepting it.
func getPositiveInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch n, err := strconv.Atoi(v); {
	case err != nil:
		warnInvalid(key, v, "is not an integer", def)
	case n <= 0:
		warnInvalid(key, v, "must be greater than 0", def)
	default:
		return n
	}
	return def
}

// getPositiveFloat reads a decimal env value that must be greater than zero,
// falling back to def and reporting anything unusable rather than accepting it.
func getPositiveFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch n, err := strconv.ParseFloat(v, 64); {
	case err != nil:
		warnInvalid(key, v, "is not a number", def)
	case n <= 0:
		warnInvalid(key, v, "must be greater than 0", def)
	default:
		return n
	}
	return def
}

// splitArgs splits a whitespace/semicolon-separated env value into args. It is
// deliberately simple; for anything exotic use per-agent adapters.
func splitList(key string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ';' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Load builds a Config from the environment.
func Load() Config {
	maxTasks := 100
	if v := os.Getenv("CLI_AGENT_MCP_MAX_TASKS"); v != "" {
		switch n, err := strconv.Atoi(v); {
		case err != nil:
			warnInvalid("CLI_AGENT_MCP_MAX_TASKS", v, "is not an integer", maxTasks)
		case n <= 0:
			warnInvalid("CLI_AGENT_MCP_MAX_TASKS", v, "must be greater than 0", maxTasks)
		default:
			maxTasks = n
		}
	}

	// Long enough to be worth blocking, short enough to return before a client
	// gives up on the call. Claude Desktop cuts a tool call at 60s unless it is
	// resetting that clock on progress notifications, and we cannot count on it.
	watchWindow := 50 * time.Second
	if v := os.Getenv("CLI_AGENT_MCP_WATCH_WINDOW_SECONDS"); v != "" {
		switch n, err := strconv.Atoi(v); {
		case err != nil:
			warnInvalid("CLI_AGENT_MCP_WATCH_WINDOW_SECONDS", v, "is not an integer", watchWindow)
		case n <= 0:
			warnInvalid("CLI_AGENT_MCP_WATCH_WINDOW_SECONDS", v, "must be greater than 0", watchWindow)
		default:
			watchWindow = time.Duration(n) * time.Second
		}
	}

	var taskTimeout time.Duration
	if v := os.Getenv("CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS"); v != "" {
		switch n, err := strconv.Atoi(v); {
		case err != nil:
			warnInvalid("CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS", v, "is not an integer", "no timeout")
		case n <= 0:
			warnInvalid("CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS", v, "must be greater than 0", "no timeout")
		default:
			taskTimeout = time.Duration(n) * time.Second
		}
	}

	allowed := splitList("CLI_AGENT_MCP_ALLOWED_CWDS")
	for i, p := range allowed {
		if abs, err := filepath.Abs(p); err == nil {
			allowed[i] = abs
		}
	}

	return Config{
		DefaultAgent:       getenv("CLI_AGENT_MCP_DEFAULT_AGENT", "claude"),
		ClaudeBin:          getenv("CLI_AGENT_MCP_CLAUDE_BIN", "claude"),
		CursorBin:          getenv("CLI_AGENT_MCP_CURSOR_BIN", "cursor-agent"),
		PermissionMode:     getenv("CLI_AGENT_MCP_PERMISSION_MODE", "acceptEdits"),
		AllowedTools:       getenv("CLI_AGENT_MCP_ALLOWED_TOOLS", ""),
		DisallowedTools:    getenv("CLI_AGENT_MCP_DISALLOWED_TOOLS", ""),
		AllowExtraArgs:     getbool("CLI_AGENT_MCP_ALLOW_EXTRA_ARGS", false),
		ClaudeExtraArgs:    splitList("CLI_AGENT_MCP_CLAUDE_EXTRA_ARGS"),
		CursorExtraArgs:    splitList("CLI_AGENT_MCP_CURSOR_EXTRA_ARGS"),
		AppendSystemPrompt: getenv("CLI_AGENT_MCP_APPEND_SYSTEM_PROMPT", ""),
		CustomName:         getenv("CLI_AGENT_MCP_CUSTOM_NAME", "custom"),
		CustomBin:          getenv("CLI_AGENT_MCP_CUSTOM_BIN", ""),
		CustomArgs:         splitList("CLI_AGENT_MCP_CUSTOM_ARGS"),
		DefaultCwd:         getenv("CLI_AGENT_MCP_DEFAULT_CWD", ""),
		AllowedCwds:        allowed,
		MaxTasks:           maxTasks,
		MaxConcurrent:      getPositiveInt("CLI_AGENT_MCP_MAX_CONCURRENT", 3),
		MaxCostUSD:         getPositiveFloat("CLI_AGENT_MCP_MAX_COST_USD", 0),
		AskPermission:      getbool("CLI_AGENT_MCP_ASK_PERMISSION", true),
		PermissionTimeout:  time.Duration(getPositiveInt("CLI_AGENT_MCP_PERMISSION_TIMEOUT_SECONDS", 600)) * time.Second,
		AuditLog:           getenv("CLI_AGENT_MCP_AUDIT_LOG", ""),
		StateDir:           getenv("CLI_AGENT_MCP_STATE_DIR", ""),
		WorktreeDir:        getenv("CLI_AGENT_MCP_WORKTREE_DIR", ""),
		WatchWindow:        watchWindow,
		TaskTimeout:        taskTimeout,
		Compact:            getbool("CLI_AGENT_MCP_COMPACT", true),
		Token:              getenv("CLI_AGENT_MCP_TOKEN", ""),
	}
}
