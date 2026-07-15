package agent

import (
	"context"
	"fmt"
	"os/exec"
)

// ClaudeAdapter drives Claude Code in headless (`-p`) mode with streaming JSON.
type ClaudeAdapter struct {
	Bin            string   // launcher; defaults handled by caller ("claude")
	PermissionMode string   // --permission-mode value
	ExtraArgs      []string // appended verbatim
}

// NewClaudeAdapter constructs a Claude Code adapter.
func NewClaudeAdapter(bin, permissionMode string, extraArgs []string) *ClaudeAdapter {
	if bin == "" {
		bin = "claude"
	}
	return &ClaudeAdapter{Bin: bin, PermissionMode: permissionMode, ExtraArgs: extraArgs}
}

func (a *ClaudeAdapter) Name() string { return "claude" }

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
	if a.PermissionMode != "" {
		args = append(args, "--permission-mode", a.PermissionMode)
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
