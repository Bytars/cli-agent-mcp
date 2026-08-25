// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"strings"
	"testing"
)

// A run stopped by its budget reports is_error with an EMPTY result, and Claude
// Code exits 0 doing it. With only those to go on the task shows up as a bare
// failure, which reads as the agent breaking rather than the limit working.
func TestBudgetExhaustionExplainsItself(t *testing.T) {
	line := `{"type":"result","is_error":true,"result":"","total_cost_usd":0.0893295,` +
		`"num_turns":1,"duration_ms":1756,"terminal_reason":"budget_exhausted",` +
		`"subtype":"error_max_budget_usd","errors":["Reached maximum budget ($0.01)"]}`

	ev := parseClaudeStreamLine(line)
	if !ev.Final || !ev.FinalError {
		t.Fatalf("expected a terminal error event, got Final=%v FinalError=%v", ev.Final, ev.FinalError)
	}
	if !strings.Contains(ev.FinalText, "Reached maximum budget") {
		t.Errorf("the agent's own explanation must survive to the caller, got %q", ev.FinalText)
	}
	if ev.Usage == nil || ev.Usage.CostUSD == 0 {
		t.Errorf("what the run cost before it was stopped must still be reported, got %+v", ev.Usage)
	}
}

// Not every stopped run names its reason in errors[]; the fallbacks have to
// produce something a person can act on rather than an empty string.
func TestTerminalReasonFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"terminal_reason only",
			`{"type":"result","is_error":true,"terminal_reason":"budget_exhausted"}`,
			"budget exhausted"},
		{"subtype only",
			`{"type":"result","is_error":true,"subtype":"error_max_turns"}`,
			"max turns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseClaudeStreamLine(tc.line).FinalText; !strings.Contains(got, tc.want) {
				t.Errorf("FinalText = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// A successful run must be left alone: the reason machinery exists for runs
// that stopped without a result, and must not overwrite one that has it.
func TestResultTextWins(t *testing.T) {
	line := `{"type":"result","is_error":false,"result":"all done","terminal_reason":"whatever"}`
	if got := parseClaudeStreamLine(line).FinalText; got != "all done" {
		t.Errorf("FinalText = %q, want the agent's actual result", got)
	}
}

// The budget reaches the agent as its own flag, because only the agent can act
// on it while a turn is still running — cost reaches us on the terminal event,
// by which point a runaway turn has already finished spending.
func TestClaudeCarriesTheBudget(t *testing.T) {
	a := NewClaudeAdapter("claude", "acceptEdits", "", "", "", nil)

	cmd, err := a.Command(context.Background(), RunSpec{Prompt: "x", MaxCostUSD: 0.25})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--max-budget-usd 0.25") {
		t.Errorf("the budget did not reach the agent: %s", args)
	}

	cmd, err = a.Command(context.Background(), RunSpec{Prompt: "x"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if args := strings.Join(cmd.Args, " "); strings.Contains(args, "--max-budget-usd") {
		t.Errorf("no budget was configured, so the flag must be absent: %s", args)
	}
}
