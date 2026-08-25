// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"strings"
	"testing"
)

// primeArgsFor builds the Prime Agent command for spec and returns its arguments.
func primeArgsFor(t *testing.T, a *PrimeAdapter, spec RunSpec) []string {
	t.Helper()
	cmd, err := a.Command(context.Background(), spec)
	if err != nil {
		t.Fatalf("Command() error: %v", err)
	}
	// cmd.Args[0] is the resolved binary (or the launcher's runtime); flags follow.
	return cmd.Args
}

// newTestPrime builds an adapter with an explicit binary so the test never
// depends on what happens to be installed on the machine running it.
func newTestPrime() *PrimeAdapter {
	return NewPrimeAdapter("pi", "", "", "", "", nil)
}

func TestPrimeName(t *testing.T) {
	if got := newTestPrime().Name(); got != "prime" {
		t.Errorf("Name() = %q, want %q", got, "prime")
	}
}

// The adapter is called "prime" but the binary defaults to "pi": the vendor
// installer and the npm package use different command names, so the default has
// to be one of the two the tool actually installs as.
func TestPrimeDefaultBinIsAKnownLauncher(t *testing.T) {
	got := NewPrimeAdapter("", "", "", "", "", nil).Bin
	for _, want := range primeBinNames {
		if got == want {
			return
		}
	}
	t.Errorf("default bin = %q, want one of %v", got, primeBinNames)
}

func TestPrimeAvailable_MissingBinNamesBothLaunchers(t *testing.T) {
	a := NewPrimeAdapter("definitely-not-installed-pi", "", "", "", "", nil)
	ok, detail := a.Available()
	if ok {
		t.Fatalf("expected an uninstalled binary to be unavailable, got detail %q", detail)
	}
	for _, name := range primeBinNames {
		if !strings.Contains(detail, name) {
			t.Errorf("detail should mention the %q launcher: %q", name, detail)
		}
	}
}

// Every run, first turn or follow-up, must be headless and machine-readable.
func TestPrimeCommand_AlwaysHeadlessJSON(t *testing.T) {
	a := newTestPrime()
	for _, spec := range []RunSpec{
		{Prompt: "do a thing"},
		{Prompt: "next", SessionID: "sess-123"},
	} {
		args := primeArgsFor(t, a, spec)
		if !hasFlag(args, "--print") {
			t.Errorf("missing --print for %+v: %v", spec, args)
		}
		if !hasFlagValue(args, "--mode", "json") {
			t.Errorf("missing --mode json for %+v: %v", spec, args)
		}
	}
}

// pi's --print swallows the next argv entry as a message unless it starts with
// "-" or "@". If a value ever landed there it would silently become a second
// prompt, so --print must always be followed immediately by a flag.
func TestPrimeCommand_PrintIsFollowedByAFlag(t *testing.T) {
	a := NewPrimeAdapter("pi", "anthropic", "bash,read", "write", "be brief", []string{"--thinking", "low"})
	args := primeArgsFor(t, a, RunSpec{Prompt: "x", Model: "sonnet", SessionID: "s1"})

	for i, arg := range args {
		if arg != "--print" {
			continue
		}
		if i+1 >= len(args) {
			t.Fatalf("--print is the last argument: %v", args)
		}
		if next := args[i+1]; !strings.HasPrefix(next, "-") {
			t.Errorf("--print is followed by %q, which pi would eat as a prompt: %v", next, args)
		}
	}
}

func TestPrimeCommand_FirstTurnHasNoSessionFlag(t *testing.T) {
	args := primeArgsFor(t, newTestPrime(), RunSpec{Prompt: "do a thing"})

	if hasFlag(args, "--session-id") {
		t.Errorf("a first turn must not name a session; pi assigns one: %v", args)
	}
	if hasFlag(args, "--session") || hasFlag(args, "--fork") {
		t.Errorf("a first turn must not reference an existing session: %v", args)
	}
}

func TestPrimeCommand_FollowupNamesTheSessionExactly(t *testing.T) {
	args := primeArgsFor(t, newTestPrime(), RunSpec{Prompt: "next", SessionID: "sess-123"})

	if !hasFlagValue(args, "--session-id", "sess-123") {
		t.Errorf("follow-up did not resume via --session-id: %v", args)
	}
	// --session takes a partial UUID and matches fuzzily; --session-id is exact.
	if hasFlag(args, "--session") {
		t.Errorf("follow-up used the fuzzy --session flag: %v", args)
	}
}

// --resume opens an interactive session picker, and --continue implicitly grabs
// "the previous session" for the project. Either one would wreck a headless run
// — the first by hanging forever waiting for a keystroke, the second by
// splicing a concurrent task's transcript into this one. Neither may ever be
// emitted, whatever the spec looks like.
func TestPrimeCommand_NeverEmitsInteractiveSessionFlags(t *testing.T) {
	cases := []struct {
		name string
		a    *PrimeAdapter
		spec RunSpec
	}{
		{"first turn", newTestPrime(), RunSpec{Prompt: "x"}},
		{"follow-up", newTestPrime(), RunSpec{Prompt: "x", SessionID: "s1"}},
		{"plan-only requested", newTestPrime(), RunSpec{Prompt: "x", PlanOnly: true}},
		{
			"fully configured",
			NewPrimeAdapter("pi", "anthropic", "bash", "write", "be brief", []string{"--thinking", "high"}),
			RunSpec{Prompt: "x", Model: "sonnet", SessionID: "s1", MCPConfigPath: "/tmp/mcp.json", PermissionTool: "approve"},
		},
	}
	banned := []string{"--resume", "-r", "--continue", "-c"}

	for _, c := range cases {
		args := primeArgsFor(t, c.a, c.spec)
		for _, flag := range banned {
			if hasFlag(args, flag) {
				t.Errorf("%s: emitted %q, which cannot work headless: %v", c.name, flag, args)
			}
		}
	}
}

func TestPrimeCommand_ProviderModelAndToolPolicy(t *testing.T) {
	a := NewPrimeAdapter("pi", "anthropic", "bash,read,edit", "write", "be brief", nil)
	args := primeArgsFor(t, a, RunSpec{Prompt: "x", Model: "sonnet"})

	for _, c := range []struct{ flag, value string }{
		{"--provider", "anthropic"},
		{"--model", "sonnet"},
		{"--tools", "bash,read,edit"},
		{"--exclude-tools", "write"},
		{"--append-system-prompt", "be brief"},
	} {
		if !hasFlagValue(args, c.flag, c.value) {
			t.Errorf("%s %s not passed: %v", c.flag, c.value, args)
		}
	}
}

func TestPrimeCommand_OmitsUnsetOptions(t *testing.T) {
	args := primeArgsFor(t, newTestPrime(), RunSpec{Prompt: "x"})

	for _, flag := range []string{"--provider", "--model", "--tools", "--exclude-tools", "--append-system-prompt"} {
		if hasFlag(args, flag) {
			t.Errorf("unset option %q should not be emitted: %v", flag, args)
		}
	}
}

// pi's --tools is a RESTRICTING allowlist, not Claude Code's pre-approval list.
// Honouring a per-run allowed_tools there would disable every tool the caller
// did not happen to name, so it must be ignored rather than merged in.
func TestPrimeCommand_PerRunAllowedToolsIgnored(t *testing.T) {
	a := NewPrimeAdapter("pi", "", "bash,read", "", "", nil)
	args := primeArgsFor(t, a, RunSpec{
		Prompt:       "x",
		AllowedTools: []string{"PowerShell", "Bash(git *)", "--dangerously-skip-permissions"},
	})

	if !hasFlagValue(args, "--tools", "bash,read") {
		t.Errorf("server tool allowlist should survive untouched: %v", args)
	}
	joined := strings.Join(args, " ")
	for _, leaked := range []string{"PowerShell", "Bash(git *)", "--dangerously-skip-permissions"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("per-run allowed_tools leaked %q into the command: %v", leaked, args)
		}
	}
}

// The prompt is the one positional message and must sit after "--", so that no
// prompt can be read as a flag and no flag as the prompt.
func TestPrimeCommand_PromptIsTheLastArgumentAfterDoubleDash(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{"plain", "do a thing", "do a thing"},
		{"leading dash", "--version is what I want you to check", "--version is what I want you to check"},
		// pi reads a positional starting with "@" as a FILE reference even after
		// "--", which would drop the instruction entirely. A leading space is
		// invisible to the model and defeats that check.
		{"leading at sign", "@src/main.go needs a review", " @src/main.go needs a review"},
	}
	a := NewPrimeAdapter("pi", "anthropic", "", "", "", []string{"--thinking", "low"})

	for _, c := range cases {
		args := primeArgsFor(t, a, RunSpec{Prompt: c.prompt, SessionID: "s1"})
		if len(args) < 2 {
			t.Fatalf("%s: not enough arguments: %v", c.name, args)
		}
		if got := args[len(args)-1]; got != c.want {
			t.Errorf("%s: last argument = %q, want %q", c.name, got, c.want)
		}
		if got := args[len(args)-2]; got != "--" {
			t.Errorf("%s: prompt is not preceded by %q, got %q: %v", c.name, "--", got, args)
		}
	}
}

func TestPrimeCommand_ExtraArgsPrecedeThePrompt(t *testing.T) {
	a := NewPrimeAdapter("pi", "", "", "", "", []string{"--thinking", "high"})
	args := primeArgsFor(t, a, RunSpec{Prompt: "x", ExtraArgs: []string{"--no-skills"}})

	if !hasFlagValue(args, "--thinking", "high") {
		t.Errorf("adapter extra args not passed: %v", args)
	}
	if !hasFlag(args, "--no-skills") {
		t.Errorf("per-run extra args not passed: %v", args)
	}
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("no %q separator: %v", "--", args)
	}
	if sep != len(args)-2 {
		t.Errorf("extra args must stay before the prompt separator: %v", args)
	}
}

func TestPrimeCommand_EmptyPromptRejected(t *testing.T) {
	if _, err := newTestPrime().Command(context.Background(), RunSpec{Prompt: ""}); err == nil {
		t.Error("expected an error for an empty prompt")
	}
}

// pi's plan mode ships as an optional extension, so the adapter cannot promise
// that a plan-only turn executes nothing. PlanCapable fails closed: refusing is
// correct, claiming it would run the task.
func TestPrimeDoesNotClaimPlanOnly(t *testing.T) {
	if CanPlan(newTestPrime()) {
		t.Error("prime must NOT claim plan-only support")
	}
}

func TestParsePrimeStreamLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want Event
	}{
		{
			name: "session header carries the id",
			line: `{"type":"session","version":3,"id":"01a03a8b-0632-73be-af60-56c08c68099a","timestamp":"2026-08-25T20:09:44.754Z","cwd":"/repo"}`,
			want: Event{SessionID: "01a03a8b-0632-73be-af60-56c08c68099a"},
		},
		{
			name: "agent_start is noise",
			line: `{"type":"agent_start"}`,
			want: Event{},
		},
		{
			name: "turn_start is noise",
			line: `{"type":"turn_start"}`,
			want: Event{},
		},
		{
			name: "message_start is noise",
			line: `{"type":"message_start","message":{"role":"assistant","content":[],"stopReason":"stop","timestamp":1}}`,
			want: Event{},
		},
		{
			// Delta-only, one event per token: surfacing it would flood the
			// transcript with fragments message_end repeats in full.
			name: "message_update deltas are dropped",
			line: `{"type":"message_update","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":12,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hel"}}`,
			want: Event{},
		},
		{
			name: "message_end surfaces assistant text only",
			line: `{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Done."},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}],"api":"anthropic-messages","provider":"anthropic","model":"claude-sonnet-4","stopReason":"toolUse","timestamp":2}}`,
			want: Event{Text: "Done."},
		},
		{
			name: "message_end for a user message says nothing",
			line: `{"type":"message_end","message":{"role":"user","content":"hi","timestamp":1}}`,
			want: Event{},
		},
		{
			name: "tool_execution_start names the tool and its arguments",
			line: `{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{"command":"git status"}}`,
			want: Event{Text: "⚙ using bash", ToolName: "bash", ToolInput: `{"command":"git status"}`},
		},
		{
			name: "tool_execution_update is dropped as partial",
			line: `{"type":"tool_execution_update","toolCallId":"c1","toolName":"bash","args":{"command":"git status"},"partialResult":{"content":[{"type":"text","text":"On branch"}]}}`,
			want: Event{},
		},
		{
			name: "tool_execution_end shows the last output line",
			line: `{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","result":{"content":[{"type":"text","text":"line1\nexit 0"}],"details":{}},"isError":false}`,
			want: Event{Text: "↳ exit 0", IsToolResult: true},
		},
		{
			name: "tool_execution_end marks failures",
			line: `{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","result":{"content":[{"type":"text","text":"boom"}],"details":{}},"isError":true}`,
			want: Event{Text: "↳ ✗ boom", IsToolResult: true, ToolResultError: true},
		},
		{
			name: "tool_execution_end with a silent success stays quiet",
			line: `{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","result":{"content":[],"details":{}},"isError":false}`,
			want: Event{IsToolResult: true},
		},
		{
			name: "agent_end is terminal and carries the answer",
			line: `{"type":"agent_end","messages":[{"role":"user","content":"hi","timestamp":1},{"role":"assistant","content":[{"type":"text","text":"All done."}],"stopReason":"stop","timestamp":2}]}`,
			want: Event{Final: true, FinalText: "All done."},
		},
		{
			name: "agent_end reports a failed turn",
			line: `{"type":"agent_end","messages":[{"role":"assistant","content":[],"stopReason":"error","errorMessage":"No API key found for the selected model.","timestamp":2}]}`,
			want: Event{Final: true, FinalError: true, FinalText: "No API key found for the selected model."},
		},
		{
			name: "agent_end reports an aborted turn with its partial text",
			line: `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"partial"}],"stopReason":"aborted","errorMessage":"aborted by user","timestamp":2}]}`,
			want: Event{Final: true, FinalError: true, FinalText: "partial\naborted by user"},
		},
		{
			name: "agent_end stays terminal with nothing to report",
			line: `{"type":"agent_end","messages":[]}`,
			want: Event{Final: true},
		},
		{
			// The run still ended, so completion must not be lost just because
			// the payload was not what we expected.
			name: "agent_end stays terminal when messages are the wrong shape",
			line: `{"type":"agent_end","messages":"nope"}`,
			want: Event{Final: true},
		},
		{
			name: "an unknown event type is left alone",
			line: `{"type":"compaction_start","reason":"threshold"}`,
			want: Event{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := c.want
			want.Raw = c.line // Raw is always preserved verbatim.

			got := parsePrimeStreamLine(c.line)
			if got != want {
				t.Errorf("parsePrimeStreamLine()\n got %#v\nwant %#v", got, want)
			}
		})
	}
}

// The audit trail needs the tool's arguments, compacted and capped.
func TestParsePrime_ToolInput(t *testing.T) {
	line := `{"type":"tool_execution_start","toolCallId":"c1","toolName":"bash","args":{ "command" : "git status" }}`
	ev := parsePrimeStreamLine(line)
	if !strings.Contains(ev.ToolInput, "git status") {
		t.Errorf("ToolInput missing the command: %q", ev.ToolInput)
	}
}

// A command that fails with NO output (e.g. killed by security software) must
// not vanish from the transcript — it should be made explicit.
func TestParsePrime_SilentToolFailureIsVisible(t *testing.T) {
	line := `{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","result":{"content":[],"details":{}},"isError":true}`
	ev := parsePrimeStreamLine(line)
	if !ev.IsToolResult || !ev.ToolResultError {
		t.Fatalf("expected a failed tool result, got IsToolResult=%v err=%v", ev.IsToolResult, ev.ToolResultError)
	}
	if !strings.Contains(ev.Text, "no output") {
		t.Errorf("silent failure must be surfaced, got Text=%q", ev.Text)
	}
	if !strings.HasPrefix(ev.Text, "↳ ✗") {
		t.Errorf("failure should be marked with ✗, got %q", ev.Text)
	}
}

// ParseLine must never panic and must say nothing it cannot back up.
func TestParsePrime_MalformedInput(t *testing.T) {
	lines := []string{
		"",
		"   ",
		"\t\n",
		"not json at all",
		"Warning: No project session found with id 'x'; creating a new session with that id.",
		"{",
		`{"type":`,
		`{"type":"session"`,
		"[1,2,3]",
		"null",
		"true",
		`"just a string"`,
		`{}`,
		`{"type":123}`,
		// isError is a string where a bool belongs: the line is unusable, so it
		// must degrade to nothing rather than half a tool result.
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","isError":"yes"}`,
		`{"type":"message_end","message":42}`,
		`{"type":"session","id":null}`,
	}

	for _, line := range lines {
		ev := parsePrimeStreamLine(line)
		if ev != (Event{Raw: line}) {
			t.Errorf("malformed line %q produced %#v, want a zero Event with Raw only", line, ev)
		}
	}
}

// ParseLine is the interface entry point; it must behave like the parser it
// delegates to.
func TestPrimeParseLine(t *testing.T) {
	line := `{"type":"session","version":3,"id":"s-1","timestamp":"t","cwd":"/repo"}`
	if got := newTestPrime().ParseLine(line); got.SessionID != "s-1" || got.Raw != line {
		t.Errorf("ParseLine() = %#v", got)
	}
}
