// Package config holds runtime configuration for the CLI-agent MCP server.
//
// All settings are read from environment variables so the server can be
// configured entirely from a Claude Desktop `mcpServers` entry, exactly like
// github-mcp-server. Every value has a sensible default.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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

	// ClaudeExtraArgs / CursorExtraArgs are appended verbatim to every launch,
	// letting the user tune flags without a rebuild.
	ClaudeExtraArgs []string
	CursorExtraArgs []string

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
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
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
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxTasks = n
		}
	}

	allowed := splitList("CLI_AGENT_MCP_ALLOWED_CWDS")
	for i, p := range allowed {
		if abs, err := filepath.Abs(p); err == nil {
			allowed[i] = abs
		}
	}

	return Config{
		DefaultAgent:    getenv("CLI_AGENT_MCP_DEFAULT_AGENT", "claude"),
		ClaudeBin:       getenv("CLI_AGENT_MCP_CLAUDE_BIN", "claude"),
		CursorBin:       getenv("CLI_AGENT_MCP_CURSOR_BIN", "cursor-agent"),
		PermissionMode:  getenv("CLI_AGENT_MCP_PERMISSION_MODE", "acceptEdits"),
		ClaudeExtraArgs: splitList("CLI_AGENT_MCP_CLAUDE_EXTRA_ARGS"),
		CursorExtraArgs: splitList("CLI_AGENT_MCP_CURSOR_EXTRA_ARGS"),
		CustomName:      getenv("CLI_AGENT_MCP_CUSTOM_NAME", "custom"),
		CustomBin:       getenv("CLI_AGENT_MCP_CUSTOM_BIN", ""),
		CustomArgs:      splitList("CLI_AGENT_MCP_CUSTOM_ARGS"),
		DefaultCwd:      getenv("CLI_AGENT_MCP_DEFAULT_CWD", ""),
		AllowedCwds:     allowed,
		MaxTasks:        maxTasks,
	}
}
