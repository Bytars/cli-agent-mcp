package agent

import (
	"strings"
	"testing"
)

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
