package agent

import (
	"context"
	"fmt"
	"os/exec"
)

// ClaudeAdapter drives Claude Code in headless (`-p`) mode with streaming JSON.
type ClaudeAdapter struct {
	Bin             string   // launcher; defaults handled by caller ("claude")
	PermissionMode  string   // --permission-mode value
	AllowedTools    string   // --allowedTools value (patterns, e.g. "Bash(git *),Edit")
	DisallowedTools string   // --disallowedTools value
	ExtraArgs       []string // appended verbatim
}

// NewClaudeAdapter constructs a Claude Code adapter.
func NewClaudeAdapter(bin, permissionMode, allowedTools, disallowedTools string, extraArgs []string) *ClaudeAdapter {
	if bin == "" {
		bin = "claude"
	}
	return &ClaudeAdapter{
		Bin:             bin,
		PermissionMode:  permissionMode,
		AllowedTools:    allowedTools,
		DisallowedTools: disallowedTools,
		ExtraArgs:       extraArgs,
	}
}

func (a *ClaudeAdapter) Name() string { return "claude" }

// SupportsPlanOnly reports that Claude Code can propose without executing, via
// --permission-mode plan.
func (a *ClaudeAdapter) SupportsPlanOnly() bool { return true }

func (a *ClaudeAdapter) Available() (bool, string) {
	if p, err := exec.LookPath(a.Bin); err == nil {
		return true, p
	}
	return false, fmt.Sprintf("%q not found in PATH (set CLI_AGENT_MCP_CLAUDE_BIN to its full path)", a.Bin)
}

func (a *ClaudeAdapter) Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error) {
	if spec.Prompt == "" {
		return nil, fmt.Errorf("claude: empty prompt")
	}
	// -p            : print/headless mode (no interactive UI)
	// stream-json   : one JSON object per line, incl. a terminal "result" event
	// --verbose     : required for stream-json to emit intermediate events
	args := []string{
		"-p", spec.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	// Plan mode overrides the configured permission mode: the point of a
	// plan-only turn is that nothing runs, so it must win.
	switch {
	case spec.PlanOnly:
		args = append(args, "--permission-mode", "plan")
	case a.PermissionMode != "":
		args = append(args, "--permission-mode", a.PermissionMode)
	}
	// Tool policy: server-side bounds on what the headless worker may do.
	if a.AllowedTools != "" {
		args = append(args, "--allowedTools", a.AllowedTools)
	}
	if a.DisallowedTools != "" {
		args = append(args, "--disallowedTools", a.DisallowedTools)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.SessionID != "" {
		// Continue an existing conversation, applying the new prompt as the turn.
		args = append(args, "--resume", spec.SessionID)
	}
	args = append(args, a.ExtraArgs...)
	args = append(args, spec.ExtraArgs...)
	return buildCommand(ctx, a.Bin, args), nil
}

func (a *ClaudeAdapter) ParseLine(line string) Event {
	return parseClaudeStreamLine(line)
}
