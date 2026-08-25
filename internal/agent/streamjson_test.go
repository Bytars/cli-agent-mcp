// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"strings"
	"testing"
)

// Claude Code has always reported what a run cost on its terminal event; the
// parser simply dropped the fields. Delegation hides everything a person at a
// terminal would have watched accumulate, and cost is the part that compounds
// without anyone noticing.
func TestParseClaude_ResultCarriesUsage(t *testing.T) {
	line := `{"type":"result","subtype":"success","session_id":"s1","is_error":false,` +
		`"result":"done","total_cost_usd":0.1234,"duration_ms":42000,"duration_api_ms":30000,"num_turns":7,` +
		`"usage":{"input_tokens":1200,"output_tokens":800,"cache_read_input_tokens":50000,"cache_creation_input_tokens":900}}`

	ev := parseClaudeStreamLine(line)
	if !ev.Final || ev.FinalText != "done" {
		t.Fatalf("expected a terminal result event, got %+v", ev)
	}
	if ev.Usage == nil {
		t.Fatal("Usage is nil; the accounting on the result event was dropped")
	}
	want := Usage{
		CostUSD: 0.1234, DurationMS: 42000, APIDurationMS: 30000, NumTurns: 7,
		InputTokens: 1200, OutputTokens: 800, CacheReadTokens: 50000, CacheWriteTokens: 900,
	}
	if *ev.Usage != want {
		t.Errorf("Usage = %+v, want %+v", *ev.Usage, want)
	}
}

// The model is only reported on the init event, and it is the only way to learn
// which model ran when the caller requested no override.
func TestParseClaude_InitCarriesModel(t *testing.T) {
	ev := parseClaudeStreamLine(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5"}`)
	if ev.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want claude-opus-5", ev.Model)
	}
	if ev.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", ev.SessionID)
	}
}

// A result with no accounting must leave Usage nil: an absent figure and a zero
// one mean different things, and rendering "$0.0000" for the first is a lie.
func TestParseClaude_ResultWithoutUsageStaysNil(t *testing.T) {
	ev := parseClaudeStreamLine(`{"type":"result","subtype":"success","is_error":false,"result":"done"}`)
	if ev.Usage != nil {
		t.Errorf("Usage = %+v, want nil when the agent reported none", *ev.Usage)
	}
}

// Turns accumulate; the agent's own turn counter is already a session total, so
// adding successive reports would inflate it.
func TestUsageAddAccumulates(t *testing.T) {
	var got Usage
	got.Add(Usage{CostUSD: 0.10, InputTokens: 100, NumTurns: 3})
	got.Add(Usage{CostUSD: 0.05, InputTokens: 50, NumTurns: 5})

	if got.CostUSD < 0.1499 || got.CostUSD > 0.1501 {
		t.Errorf("CostUSD = %v, want the two turns summed", got.CostUSD)
	}
	if got.InputTokens != 150 {
		t.Errorf("InputTokens = %d, want 150", got.InputTokens)
	}
	if got.NumTurns != 5 {
		t.Errorf("NumTurns = %d, want the latest session total (5), not the sum", got.NumTurns)
	}
}

func TestParseClaude_ToolUse(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git status"}}]}}`
	ev := parseClaudeStreamLine(line)
	if ev.ToolName != "Bash" {
		t.Errorf("ToolName = %q, want Bash", ev.ToolName)
	}
	if !strings.Contains(ev.ToolInput, "git status") {
		t.Errorf("ToolInput missing command: %q", ev.ToolInput)
	}
	if !strings.Contains(ev.Text, "⚙ using Bash") {
		t.Errorf("Text = %q", ev.Text)
	}
}

func TestParseClaude_ToolResultOK(t *testing.T) {
	line := `{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"line1\nexit 0"}]}]}}`
	ev := parseClaudeStreamLine(line)
	if !ev.IsToolResult || ev.ToolResultError {
		t.Errorf("expected a non-error tool result, got IsToolResult=%v err=%v", ev.IsToolResult, ev.ToolResultError)
	}
	if ev.Text != "↳ exit 0" {
		t.Errorf("Text = %q, want the last output line", ev.Text)
	}
}

// A command that fails with NO output (e.g. killed by security software) must
// not vanish from the transcript — it should be made explicit.
func TestParseClaude_SilentToolFailureIsVisible(t *testing.T) {
	line := `{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":[]}]}}`
	ev := parseClaudeStreamLine(line)
	if !ev.IsToolResult || !ev.ToolResultError {
		t.Fatalf("expected a failed tool result, got IsToolResult=%v err=%v", ev.IsToolResult, ev.ToolResultError)
	}
	if ev.Text == "" || !strings.Contains(ev.Text, "no output") {
		t.Errorf("silent failure must be surfaced, got Text=%q", ev.Text)
	}
	if !strings.HasPrefix(ev.Text, "↳ ✗") {
		t.Errorf("failure should be marked with ✗, got %q", ev.Text)
	}
}

func TestParseClaude_SystemNoiseIsEmpty(t *testing.T) {
	// System/init/rate-limit lines carry no human-facing text (so compact mode
	// drops them).
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"x","tools":["Bash"]}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"}]}}`,
	} {
		if ev := parseClaudeStreamLine(line); ev.Text != "" {
			t.Errorf("expected noise line to have empty Text, got %q for %s", ev.Text, line)
		}
	}
}
