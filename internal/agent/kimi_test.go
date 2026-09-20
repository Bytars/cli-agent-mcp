// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"strings"
	"testing"
)

// kimiArgsFor builds the Kimi command for spec and returns its arguments.
func kimiArgsFor(t *testing.T, a *KimiAdapter, spec RunSpec) []string {
	t.Helper()
	cmd, err := a.Command(context.Background(), spec)
	if err != nil {
		t.Fatalf("Command() error: %v", err)
	}
	return cmd.Args
}

func TestKimiCommand_Defaults(t *testing.T) {
	a := NewKimiAdapter("kimi", nil)
	args := kimiArgsFor(t, a, RunSpec{Prompt: "do a thing"})

	if !hasFlagValue(args, "-p", "do a thing") {
		t.Errorf("prompt not passed via -p: %v", args)
	}
	if !hasFlagValue(args, "--output-format", "stream-json") {
		t.Errorf("missing stream-json output format: %v", args)
	}
}

func TestKimiCommand_ModelAndSession(t *testing.T) {
	a := NewKimiAdapter("kimi", nil)
	args := kimiArgsFor(t, a, RunSpec{
		Prompt:    "follow up",
		Model:     "k2d8-preview",
		SessionID: "session_ab9df05f-ebe0-4d72-8991-dfc89e17640d",
	})

	if !hasFlagValue(args, "--model", "k2d8-preview") {
		t.Errorf("model override not applied: %v", args)
	}
	if !hasFlagValue(args, "--session", "session_ab9df05f-ebe0-4d72-8991-dfc89e17640d") {
		t.Errorf("session resume not applied: %v", args)
	}
}

func TestKimiCommand_ExtraArgsLast(t *testing.T) {
	a := NewKimiAdapter("kimi", []string{"--thinking"})
	args := kimiArgsFor(t, a, RunSpec{Prompt: "x", ExtraArgs: []string{"--foo"}})
	if !hasFlag(args, "--thinking") || !hasFlag(args, "--foo") {
		t.Errorf("extra args not appended: %v", args)
	}
	// Extra args must come after the built-in flags so nothing overrides -p.
	pIdx, fooIdx := -1, -1
	for i, ar := range args {
		if ar == "-p" && pIdx == -1 {
			pIdx = i
		}
		if ar == "--foo" {
			fooIdx = i
		}
	}
	if pIdx == -1 || fooIdx < pIdx {
		t.Errorf("extra args should follow built-ins: %v", args)
	}
}

// Kimi's --plan cannot be combined with -p, so plan-only must fail closed at
// the CanPlan gate, not silently execute.
func TestKimiAdapter_NoPlanOnly(t *testing.T) {
	a := NewKimiAdapter("kimi", nil)
	if CanPlan(a) {
		t.Error("kimi must not report plan-only support: --plan is incompatible with -p")
	}
}

func TestKimiAdapter_NoOutputAsResult(t *testing.T) {
	a := NewKimiAdapter("kimi", nil)
	// Kimi must NOT use the raw-lines fallback: its stdout is JSONL, so the
	// result would be raw protocol noise. The manager's lastText fallback (the
	// final assistant message) is the correct result for this schema.
	if r, ok := interface{}(a).(ResultFromOutput); ok && r.UseOutputAsResult() {
		t.Error("kimi stdout is JSONL; using raw output as result would leak protocol noise")
	}
}

// Real lines captured from kimi 2.0.2 (win32-x64) with --output-format stream-json.
func TestKimiParseLine_Measured(t *testing.T) {
	a := NewKimiAdapter("kimi", nil)

	// Init noise: no text, no session.
	ev := a.ParseLine(`{"role":"meta","type":"system.version","version":"2.0.2"}`)
	if ev.Text != "" || ev.SessionID != "" || ev.Final {
		t.Errorf("system.version should be noise: %+v", ev)
	}

	// Assistant text.
	ev = a.ParseLine(`{"role":"assistant","content":"OK"}`)
	if ev.Text != "OK" {
		t.Errorf("assistant text not extracted: %+v", ev)
	}

	// Tool call: name + input for the audit trail, ⚙ line for progress.
	ev = a.ParseLine(`{"role":"assistant","tool_calls":[{"type":"function","id":"tool_A65lmud3i4eSH5LnxAQFi1we","function":{"name":"Glob","arguments":"{\"pattern\":\"*.md\",\"head_limit\":5}"}}]}`)
	if ev.ToolName != "Glob" {
		t.Errorf("tool name not extracted: %+v", ev)
	}
	if !strings.Contains(ev.ToolInput, `"pattern":"*.md"`) {
		t.Errorf("tool input not captured: %q", ev.ToolInput)
	}
	if !strings.Contains(ev.Text, "⚙ using Glob") {
		t.Errorf("tool progress line missing: %q", ev.Text)
	}

	// Tool result: last line surfaced with the ↳ prefix.
	ev = a.ParseLine(`{"role":"tool","tool_call_id":"tool_A65lmud3i4eSH5LnxAQFi1we","content":"Showing matches 1–5 of 1122.\nexchange/estado/2026-09-20-continuidad-frente-ia-bridge-vs-livekit.md"}`)
	if !ev.IsToolResult {
		t.Errorf("tool result not flagged: %+v", ev)
	}
	if ev.ToolResultError {
		t.Errorf("clean tool result marked as error: %+v", ev)
	}
	if !strings.HasPrefix(ev.Text, "↳ ") || !strings.Contains(ev.Text, "bridge-vs-livekit.md") {
		t.Errorf("tool result text wrong: %q", ev.Text)
	}

	// Resume hint: session id captured, no transcript noise.
	ev = a.ParseLine(`{"role":"meta","type":"session.resume_hint","session_id":"session_ab9df05f-ebe0-4d72-8991-dfc89e17640d","command":"kimi -r session_ab9df05f-ebe0-4d72-8991-dfc89e17640d","content":"To resume this session: kimi -r session_ab9df05f-ebe0-4d72-8991-dfc89e17640d"}`)
	if ev.SessionID != "session_ab9df05f-ebe0-4d72-8991-dfc89e17640d" {
		t.Errorf("session id not captured: %+v", ev)
	}
	if ev.Text != "" {
		t.Errorf("resume hint should not render as text: %q", ev.Text)
	}
}

func TestKimiParseLine_Tolerant(t *testing.T) {
	a := NewKimiAdapter("kimi", nil)

	// Non-JSON line: surfaced as plain text.
	ev := a.ParseLine("some plain chatter")
	if ev.Text != "some plain chatter" {
		t.Errorf("plain line dropped: %+v", ev)
	}

	// Garbage JSON: kept in Raw, nothing panics.
	ev = a.ParseLine(`{"role":"assistant","content":`)
	if ev.Raw == "" || ev.Text != "" {
		t.Errorf("malformed line mishandled: %+v", ev)
	}

	// Error meta: surfaced.
	ev = a.ParseLine(`{"role":"meta","type":"run.error","content":"boom"}`)
	if !strings.Contains(ev.Text, "boom") {
		t.Errorf("error meta not surfaced: %+v", ev)
	}

	// Errored tool result with no output: made explicit, not silent.
	ev = a.ParseLine(`{"role":"tool","tool_call_id":"t1","is_error":true,"content":""}`)
	if !ev.IsToolResult || !ev.ToolResultError || !strings.Contains(ev.Text, "no output") {
		t.Errorf("silent tool failure not surfaced: %+v", ev)
	}
}
