package agent

import (
	"context"
	"strings"
	"testing"
)

// argsFor builds the Claude command for spec and returns its arguments.
func argsFor(t *testing.T, a *ClaudeAdapter, spec RunSpec) []string {
	t.Helper()
	cmd, err := a.Command(context.Background(), spec)
	if err != nil {
		t.Fatalf("Command() error: %v", err)
	}
	// cmd.Args[0] is the resolved binary (or the wrapper); flags follow.
	return cmd.Args
}

// hasFlagValue reports whether args contains flag immediately followed by value.
func hasFlagValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func TestClaudeCommand_Defaults(t *testing.T) {
	a := NewClaudeAdapter("claude", "acceptEdits", "", "", nil)
	args := argsFor(t, a, RunSpec{Prompt: "do a thing"})

	if !hasFlagValue(args, "-p", "do a thing") {
		t.Errorf("prompt not passed via -p: %v", args)
	}
	if !hasFlagValue(args, "--output-format", "stream-json") {
		t.Errorf("missing stream-json output format: %v", args)
	}
	if !hasFlag(args, "--verbose") {
		t.Errorf("stream-json requires --verbose: %v", args)
	}
	if !hasFlagValue(args, "--permission-mode", "acceptEdits") {
		t.Errorf("configured permission mode not applied: %v", args)
	}
}

// Plan mode must win over the configured permission mode: a plan-only turn that
// silently ran under bypassPermissions would execute the very thing the caller
// asked it to only propose.
func TestClaudeCommand_PlanOnlyOverridesPermissionMode(t *testing.T) {
	a := NewClaudeAdapter("claude", "bypassPermissions", "", "", nil)
	args := argsFor(t, a, RunSpec{Prompt: "delete prod", PlanOnly: true})

	if !hasFlagValue(args, "--permission-mode", "plan") {
		t.Errorf("plan-only did not force --permission-mode plan: %v", args)
	}
	if hasFlagValue(args, "--permission-mode", "bypassPermissions") {
		t.Errorf("configured permission mode leaked into a plan-only run: %v", args)
	}
	if strings.Count(strings.Join(args, " "), "--permission-mode") != 1 {
		t.Errorf("expected exactly one --permission-mode: %v", args)
	}
}

func TestClaudeCommand_ToolPolicy(t *testing.T) {
	a := NewClaudeAdapter("claude", "acceptEdits", "Bash(git *),Edit", "Bash(rm *)", nil)
	args := argsFor(t, a, RunSpec{Prompt: "x"})

	if !hasFlagValue(args, "--allowedTools", "Bash(git *),Edit") {
		t.Errorf("allowlist not passed: %v", args)
	}
	if !hasFlagValue(args, "--disallowedTools", "Bash(rm *)") {
		t.Errorf("denylist not passed: %v", args)
	}
}

func TestClaudeCommand_OmitsEmptyToolPolicy(t *testing.T) {
	a := NewClaudeAdapter("claude", "acceptEdits", "", "", nil)
	args := argsFor(t, a, RunSpec{Prompt: "x"})

	if hasFlag(args, "--allowedTools") || hasFlag(args, "--disallowedTools") {
		t.Errorf("empty tool policy should not emit flags: %v", args)
	}
}

func TestClaudeCommand_Resume(t *testing.T) {
	a := NewClaudeAdapter("claude", "acceptEdits", "", "", nil)
	args := argsFor(t, a, RunSpec{Prompt: "next", SessionID: "sess-123"})

	if !hasFlagValue(args, "--resume", "sess-123") {
		t.Errorf("session not resumed: %v", args)
	}
}

func TestClaudeCommand_EmptyPromptRejected(t *testing.T) {
	a := NewClaudeAdapter("claude", "acceptEdits", "", "", nil)
	if _, err := a.Command(context.Background(), RunSpec{Prompt: ""}); err == nil {
		t.Error("expected an error for an empty prompt")
	}
}

// Plan capability must be explicit, and must fail closed for agents that cannot
// guarantee it — otherwise agent_plan_task would execute the task.
func TestCanPlan(t *testing.T) {
	if !CanPlan(NewClaudeAdapter("claude", "acceptEdits", "", "", nil)) {
		t.Error("claude should support plan-only")
	}
	if !CanPlan(NewMockAdapter()) {
		t.Error("mock should support plan-only")
	}
	if CanPlan(NewCursorAdapter("cursor-agent", nil)) {
		t.Error("cursor must NOT claim plan-only support")
	}
	if CanPlan(NewCustomAdapter("custom", "echo", []string{"{{prompt}}"})) {
		t.Error("a custom CLI must NOT claim plan-only support")
	}
}
