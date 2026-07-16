package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// MockAdapter is a self-contained agent used for testing the full
// spawn → stream → parse → complete pipeline without Claude Code or Cursor
// installed. It re-invokes this very binary's hidden `__mock` subcommand, which
// emits Claude-compatible stream-json.
type MockAdapter struct {
	selfExe string
}

// NewMockAdapter builds a mock adapter bound to the current executable.
func NewMockAdapter() *MockAdapter {
	exe, _ := os.Executable()
	return &MockAdapter{selfExe: exe}
}

func (a *MockAdapter) Name() string { return "mock" }

func (a *MockAdapter) Available() (bool, string) {
	if a.selfExe == "" {
		return false, "could not resolve own executable path"
	}
	return true, a.selfExe + " __mock"
}

// SupportsPlanOnly lets the mock exercise the plan-only path end to end.
func (a *MockAdapter) SupportsPlanOnly() bool { return true }

func (a *MockAdapter) Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error) {
	if a.selfExe == "" {
		return nil, fmt.Errorf("mock: unknown executable path")
	}
	args := []string{"__mock", "-p", spec.Prompt}
	if spec.PlanOnly {
		args = append(args, "--plan")
	}
	return exec.CommandContext(ctx, a.selfExe, args...), nil
}

func (a *MockAdapter) ParseLine(line string) Event { return parseClaudeStreamLine(line) }

// RunMock implements the `__mock` subcommand: it prints a short, deterministic
// Claude-style stream-json transcript for the given prompt and exits 0.
func RunMock(args []string) int {
	prompt := "no prompt"
	planOnly := false
	for i := 0; i < len(args); i++ {
		if args[i] == "-p" && i+1 < len(args) {
			prompt = args[i+1]
		}
		if args[i] == "--plan" {
			planOnly = true
		}
	}
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(os.Stdout, string(b))
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": "mock-session-1"})

	// Plan-only: propose, execute nothing.
	if planOnly {
		emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "Planning: " + prompt}},
			},
		})
		emit(map[string]any{
			"type":       "result",
			"subtype":    "success",
			"session_id": "mock-session-1",
			"is_error":   false,
			"result":     "PLAN for: " + prompt + "\n1. inspect\n2. change\n3. verify",
		})
		return 0
	}
	// Assistant decides to run a tool.
	emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "make build"}}},
		},
	})
	// Tool result (multi-line output; the last line is what progress should show).
	emit(map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": "t1",
				"content":     []map[string]any{{"type": "text", "text": "compiling...\nlinking...\nexit status 0"}},
			}},
		},
	})
	emit(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []map[string]any{{"type": "text", "text": "Working on: " + prompt}},
		},
	})
	emit(map[string]any{
		"type":       "result",
		"subtype":    "success",
		"session_id": "mock-session-1",
		"is_error":   false,
		"result":     "Completed task: " + prompt,
	})
	return 0
}
