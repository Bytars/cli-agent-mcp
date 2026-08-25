// SPDX-License-Identifier: Apache-2.0

package task

import (
	"testing"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
)

// The figure handed to a turn has to be what the task has LEFT. The agent
// applies it per invocation, so passing the configured total every time would
// let a task driven through ten follow-ups spend ten budgets while every
// individual run stayed inside the limit.
func TestBudgetShrinksAsATaskSpends(t *testing.T) {
	m := NewManager(10)
	m.SetMaxCostUSD(1.00)

	tk := m.newTask(agent.NewMockAdapter(), t.TempDir(), agent.RunSpec{Prompt: "x"})

	left, ok := m.budgetFor(tk)
	if !ok || left != 1.00 {
		t.Fatalf("a fresh task should get the whole budget, got %v (ok=%v)", left, ok)
	}

	tk.usage.Add(agent.Usage{CostUSD: 0.60})
	switch left, ok = m.budgetFor(tk); {
	case !ok:
		t.Fatal("a task under budget must still be allowed to run")
	case left > 0.4001 || left < 0.3999:
		t.Errorf("remaining budget = %v, want the unspent 0.40", left)
	}
}

// At the limit the turn is refused rather than started with an allowance of
// zero, which the agent reads as "no limit" — the one reading that turns a
// budget into its opposite.
func TestBudgetRefusesOnceSpent(t *testing.T) {
	m := NewManager(10)
	m.SetMaxCostUSD(1.00)
	tk := m.newTask(agent.NewMockAdapter(), t.TempDir(), agent.RunSpec{Prompt: "x"})

	for _, spent := range []float64{1.00, 1.50} {
		tk.usage = agent.Usage{CostUSD: spent}
		if left, ok := m.budgetFor(tk); ok {
			t.Errorf("spent $%.2f of $1.00: expected a refusal, got allowed with $%v left", spent, left)
		}
	}
}

// Zero is the documented way to turn the limit off and must not be read as
// "nothing may be spent".
func TestZeroBudgetMeansNoLimit(t *testing.T) {
	m := NewManager(10)
	m.SetMaxCostUSD(0)
	tk := m.newTask(agent.NewMockAdapter(), t.TempDir(), agent.RunSpec{Prompt: "x"})
	tk.usage.Add(agent.Usage{CostUSD: 999})

	left, ok := m.budgetFor(tk)
	if !ok {
		t.Fatal("an unlimited manager refused a task")
	}
	if left != 0 {
		t.Errorf("with no limit the turn must carry no budget flag, got %v", left)
	}
}

// A snapshot that reported nothing is different from one that reported free:
// the caller has to be able to tell "not measured" from "cost zero".
func TestSnapshotDistinguishesUnmeasuredFromFree(t *testing.T) {
	m := NewManager(10)
	tk := m.newTask(agent.NewMockAdapter(), t.TempDir(), agent.RunSpec{Prompt: "x"})

	if snap := tk.Snapshot(); snap.Usage != nil {
		t.Errorf("a task that reported no accounting must leave Usage nil, got %+v", snap.Usage)
	}

	tk.usage.Add(agent.Usage{CostUSD: 0.25, InputTokens: 10})
	tk.modelUsed = "claude-opus-5"
	snap := tk.Snapshot()
	if snap.Usage == nil {
		t.Fatal("accounting was reported but the snapshot dropped it")
	}
	if snap.Usage.CostUSD != 0.25 || snap.ModelUsed != "claude-opus-5" {
		t.Errorf("snapshot lost the accounting: %+v model=%q", snap.Usage, snap.ModelUsed)
	}
}
