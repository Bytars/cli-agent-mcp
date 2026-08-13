// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ClaudeAdapter drives Claude Code in headless (`-p`) mode with streaming JSON.
type ClaudeAdapter struct {
	Bin             string   // launcher; defaults handled by caller ("claude")
	PermissionMode  string   // --permission-mode value
	AllowedTools    string   // --allowedTools value (patterns, e.g. "Bash(git *),Edit")
	DisallowedTools string   // --disallowedTools value
	AppendPrompt    string   // --append-system-prompt value (standing guidance)
	ExtraArgs       []string // appended verbatim
}

// NewClaudeAdapter constructs a Claude Code adapter.
func NewClaudeAdapter(bin, permissionMode, allowedTools, disallowedTools, appendPrompt string, extraArgs []string) *ClaudeAdapter {
	if bin == "" {
		bin = "claude"
	}
	return &ClaudeAdapter{
		Bin:             bin,
		PermissionMode:  permissionMode,
		AllowedTools:    allowedTools,
		DisallowedTools: disallowedTools,
		AppendPrompt:    appendPrompt,
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
	// Tool policy. --allowedTools PRE-APPROVES tools (avoids headless deadlocks
	// where a tool would otherwise wait for approval no one can give); it does
	// not restrict. We merge the server's configured allowlist with any per-run
	// request, joined into a single argument so a value can never be parsed as a
	// flag (no injection via allowed_tools). --disallowedTools is the real deny
	// gate and is server-only.
	if allowed := mergeAllowed(a.AllowedTools, spec.AllowedTools); allowed != "" {
		args = append(args, "--allowedTools", allowed)
	}
	if a.DisallowedTools != "" {
		args = append(args, "--disallowedTools", a.DisallowedTools)
	}
	if a.AppendPrompt != "" {
		args = append(args, "--append-system-prompt", a.AppendPrompt)
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
	return buildCommand(ctx, a.Bin, args)
}

func (a *ClaudeAdapter) ParseLine(line string) Event {
	return parseClaudeStreamLine(line)
}

// mergeAllowed combines the server-configured allowlist (a comma-separated
// string) with a per-run list, into one comma-joined value. Entries that look
// like flags (start with '-') or are blank are dropped, so nothing passed here
// can turn into a CLI flag.
func mergeAllowed(configured string, perRun []string) string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "-") {
			return
		}
		out = append(out, s)
	}
	for _, s := range strings.Split(configured, ",") {
		add(s)
	}
	for _, s := range perRun {
		add(s)
	}
	return strings.Join(out, ",")
}
