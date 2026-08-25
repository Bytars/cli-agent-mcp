// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// PrimeAdapter drives Prime Agent (github.com/PrimeIntellect-ai/prime-agent) in
// headless print mode with its JSON event stream (`--print --mode json`).
//
// Adapter name vs. binary name: the adapter registers as "prime" but launches
// `pi`. The tool genuinely ships under both names — the vendor's own installer
// puts a `prime-agent` command on PATH, while the npm package
// (@earendil-works/pi-coding-agent) declares its bin as `pi` — so either is
// plausible on a given machine. "prime" is the name a caller would reach for;
// `pi` is what is most likely to be installed, so it is the default launcher,
// with `prime-agent` picked up automatically when only that one exists.
type PrimeAdapter struct {
	Bin          string   // launcher; defaults to "pi" (see above)
	Provider     string   // --provider value (pi's own default is "google")
	Tools        string   // --tools value: comma-separated allowlist of tool names
	ExcludeTools string   // --exclude-tools value: comma-separated denylist
	AppendPrompt string   // --append-system-prompt value (standing guidance)
	ExtraArgs    []string // appended verbatim
}

// primeBinNames are the launcher names Prime Agent installs under, preferred
// first.
var primeBinNames = []string{"pi", "prime-agent"}

// NewPrimeAdapter constructs a Prime Agent adapter. An empty bin picks whichever
// of the two launcher names is actually installed, falling back to "pi" so the
// unavailable case still reports a concrete name.
func NewPrimeAdapter(bin, provider, tools, excludeTools, appendPrompt string, extraArgs []string) *PrimeAdapter {
	if bin == "" {
		bin = defaultPrimeBin()
	}
	return &PrimeAdapter{
		Bin:          bin,
		Provider:     provider,
		Tools:        tools,
		ExcludeTools: excludeTools,
		AppendPrompt: appendPrompt,
		ExtraArgs:    extraArgs,
	}
}

func defaultPrimeBin() string {
	for _, n := range primeBinNames {
		if _, err := exec.LookPath(n); err == nil {
			return n
		}
	}
	return primeBinNames[0]
}

func (a *PrimeAdapter) Name() string { return "prime" }

// Note there is deliberately no SupportsPlanOnly here. pi's plan mode arrives as
// an *extension* (`--plan`, from the plan-mode extension) rather than a core
// flag: it may not be installed, an unrecognized flag is not a hard error, and
// even when present it is not a guarantee that nothing executes. PlanCapable
// must fail closed — claiming it would make agent_plan_task run the very task
// the caller asked it only to propose.

func (a *PrimeAdapter) Available() (bool, string) {
	if p, err := exec.LookPath(a.Bin); err == nil {
		return true, p
	}
	// Presence on PATH is all we can honestly check. pi resolves a provider key
	// lazily and only complains once a model call is attempted ("No API key
	// found for the selected model", on stderr, with a non-zero exit), so there
	// is no credential probe to run here that would not also cost a request.
	return false, fmt.Sprintf("%q not found in PATH (Prime Agent installs as either %q or %q; set CLI_AGENT_MCP_PRIME_BIN to its full path)",
		a.Bin, primeBinNames[0], primeBinNames[1])
}

func (a *PrimeAdapter) Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error) {
	if spec.Prompt == "" {
		return nil, fmt.Errorf("prime: empty prompt")
	}
	// --print     : non-interactive — process the prompt and exit.
	// --mode json : one JSON object per line, opening with the session header.
	//
	// Order matters: --print greedily swallows the NEXT argv entry as a message
	// unless that entry starts with "-" or "@", so it is always followed
	// immediately by a flag and never by anything we care about.
	args := []string{"--print", "--mode", "json"}

	if a.Provider != "" {
		args = append(args, "--provider", a.Provider)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	// Tool policy. Unlike Claude Code's --allowedTools, which *pre-approves*
	// tools so a headless run cannot deadlock waiting for consent, pi's --tools
	// is a restricting allowlist of tool names: passing it removes everything
	// not listed. That is why spec.AllowedTools is deliberately NOT merged in
	// here — a per-run request to pre-approve `Bash(git *)` would, under pi,
	// disable read/write/edit and quietly cripple the run. pi has no headless
	// approval prompt to deadlock on in the first place, so there is nothing for
	// a per-run allowlist to buy. Both flags stay server-configured only.
	if a.Tools != "" {
		args = append(args, "--tools", a.Tools)
	}
	if a.ExcludeTools != "" {
		args = append(args, "--exclude-tools", a.ExcludeTools)
	}
	if a.AppendPrompt != "" {
		args = append(args, "--append-system-prompt", a.AppendPrompt)
	}
	// spec.MCPConfigPath / spec.PermissionTool have no counterpart: pi exposes
	// no approval-prompt hook, so an approval endpoint would never be called.
	// Leaving them unwired is honest; inventing a flag would not be.

	// Session handling. pi offers four ways to pick a conversation back up and
	// only one of them is usable from a server:
	//
	//   --resume     opens an INTERACTIVE session picker. Nothing headless can
	//                answer it, so the process would sit there until killed.
	//                Never emit it, under any circumstances.
	//   --continue   implicitly takes "the previous session" for this project.
	//                With several tasks running against one repository that is a
	//                coin flip, and losing it splices two transcripts together.
	//   --session    matches a path or a *partial* UUID — fuzzy, so it can
	//                resolve to a neighbouring session.
	//   --session-id names one session exactly, and creates it when missing.
	//
	// So follow-ups pass --session-id and the first turn passes no session flag
	// at all. Because --session-id creates on miss, we could equally mint an id
	// up front and use the identical flag on every turn. We don't: pi prints
	// "Warning: No project session found with id '...'; creating a new session
	// with that id." to stderr whenever the named session does not exist yet,
	// and the task manager surfaces stderr in the user-visible transcript —
	// which would stamp a spurious warning on the first turn of every task. And
	// pre-assignment buys nothing, because the manager only learns a session id
	// by observing one in the stream either way: --mode json always opens with a
	// {"type":"session","id":...} header, ParseLine lifts the id out of it, and
	// the manager hands it back here on the next turn.
	if spec.SessionID != "" {
		args = append(args, "--session-id", spec.SessionID)
	}

	args = append(args, a.ExtraArgs...)
	args = append(args, spec.ExtraArgs...)

	// "--" ends option parsing; everything after it is a message. The prompt goes
	// last so that no configured flag can be mistaken for it, and no prompt can
	// be mistaken for a flag.
	args = append(args, "--", primePromptArg(spec.Prompt))
	return buildCommand(ctx, a.Bin, args)
}

// primePromptArg prepares the prompt for pi's positional-message slot.
//
// Everything after "--" is a message, with one exception: pi reads an entry
// beginning with "@" as a FILE reference rather than as text. A task that
// happened to start with "@" would become a read of a file that does not exist,
// and the agent would run with no instruction at all — a silent failure that
// looks like the agent simply ignored the request. pi offers no escape for this,
// so shift such a prompt by one leading space: pi's "@" check sees the space,
// the model does not.
func primePromptArg(prompt string) string {
	if strings.HasPrefix(prompt, "@") {
		return " " + prompt
	}
	return prompt
}

func (a *PrimeAdapter) ParseLine(line string) Event {
	return parsePrimeStreamLine(line)
}

// primeEvent is the subset of pi's JSON event stream we read. The field names
// come from the upstream sources: the event envelope from
// packages/agent/src/types.ts (AgentEvent) together with the coding agent's
// JsonAgentSessionEvent, and the session header from
// packages/coding-agent/docs/json.md. Note the tool fields are camelCase
// ("toolName", "isError"), unlike Claude Code's snake_case stream.
//
// "messages" is kept raw rather than typed as a slice so that one unexpected
// shape cannot fail the whole line's decode.
type primeEvent struct {
	Type string `json:"type"`

	ID string `json:"id"` // session header: the session id

	Message  json.RawMessage `json:"message"`  // message_start / message_end / turn_end
	Messages json.RawMessage `json:"messages"` // agent_end: every message of the run

	ToolName string          `json:"toolName"` // tool_execution_*
	Args     json.RawMessage `json:"args"`     // tool_execution_start / _update
	Result   json.RawMessage `json:"result"`   // tool_execution_end
	IsError  bool            `json:"isError"`  // tool_execution_end
}

// primeMessage is the subset of pi's AgentMessage we read, confirmed against
// packages/ai/src/types.ts (UserMessage / AssistantMessage / ToolResultMessage).
//
// Content is raw because a user message's content may be a bare string while an
// assistant message's is always an array of blocks; decoding it eagerly into a
// slice would fail the whole message on the string form.
type primeMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
}

// primeContent is one assistant content block. Its "type" is "text",
// "thinking", "toolCall" or "image" (packages/ai/src/types.ts); only the text
// blocks carry anything we surface.
type primeContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parsePrimeStreamLine(line string) Event {
	ev := Event{Raw: line}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return ev
	}
	var pe primeEvent
	if err := json.Unmarshal([]byte(trimmed), &pe); err != nil {
		return ev
	}

	switch pe.Type {
	case "session":
		// The header line, always first. This is the only place a session id is
		// reported, and capturing it here is what makes a follow-up possible.
		ev.SessionID = pe.ID

	case "message_end":
		// The authoritative assistant message for one response. message_update is
		// skipped on purpose: it is delta-only (one event per token), so
		// surfacing it would flood the transcript with fragments of the text that
		// message_end then repeats in full.
		m, ok := primeDecodeMessage(pe.Message)
		if ok && m.Role == "assistant" {
			ev.Text = primeText(m)
		}

	case "tool_execution_start":
		// The canonical tool-use record: it carries the arguments, and it fires
		// once per call. The tool-call blocks inside message_end name the same
		// calls, so they are left alone to avoid double-reporting.
		if pe.ToolName != "" {
			ev.ToolName = pe.ToolName
			ev.ToolInput = truncateJSON(pe.Args, 400)
			ev.Text = "⚙ using " + pe.ToolName
		}

	case "tool_execution_end":
		ev.IsToolResult = true
		ev.ToolResultError = pe.IsError
		last := lastNonEmptyLine(primeResultText(pe.Result))
		if last == "" {
			// Surface silent failures: a command that failed with no output
			// (e.g. killed by security software) would otherwise vanish.
			if !pe.IsError {
				return ev
			}
			last = "(failed with no output — possibly blocked by security software / sandbox)"
		}
		prefix := "↳ "
		if pe.IsError {
			prefix = "↳ ✗ "
		}
		ev.Text = prefix + last

	case "agent_end":
		// The terminal event: one run ends with exactly one agent_end. Note that
		// in JSON mode pi exits 0 even for a failed turn, so the error state has
		// to come from the last assistant message's stopReason rather than from
		// the exit code.
		ev.Final = true
		if m, ok := primeLastAssistant(pe.Messages); ok {
			var b strings.Builder
			appendPart(&b, primeText(m))
			if m.StopReason == "error" || m.StopReason == "aborted" {
				ev.FinalError = true
				appendPart(&b, strings.TrimSpace(m.ErrorMessage))
			}
			ev.FinalText = b.String()
		}
	}
	// Everything else — agent_start, turn_start, turn_end, message_start,
	// message_update, tool_execution_update, session_action_update, queue_update,
	// compaction_* and auto_retry_* — carries nothing this Event can represent
	// and is left zero-valued rather than guessed at. pi's per-message `usage`
	// (tokens and cost) and the assistant message's `model` are reported by the
	// stream but have no field on Event, so they are dropped here too.
	return ev
}

// primeDecodeMessage decodes one AgentMessage, reporting whether it parsed.
func primeDecodeMessage(raw json.RawMessage) (primeMessage, bool) {
	var m primeMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return primeMessage{}, false
	}
	return m, true
}

// primeLastAssistant finds the final assistant message in an agent_end payload,
// which is the one holding the run's answer and its stop reason.
func primeLastAssistant(raw json.RawMessage) (primeMessage, bool) {
	if len(raw) == 0 {
		return primeMessage{}, false
	}
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return primeMessage{}, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if m, ok := primeDecodeMessage(msgs[i]); ok && m.Role == "assistant" {
			return m, true
		}
	}
	return primeMessage{}, false
}

// primeText joins a message's text blocks. Thinking blocks are omitted (they are
// reasoning, not an answer) and tool-call blocks are left to the
// tool_execution_start events, which report the same calls with their arguments.
func primeText(m primeMessage) string {
	var b strings.Builder
	for _, c := range primeBlocks(m) {
		if c.Type == "text" {
			appendPart(&b, c.Text)
		}
	}
	return b.String()
}

// primeBlocks normalises a message's content into blocks, accepting the bare
// string form a user message may use.
func primeBlocks(m primeMessage) []primeContent {
	if len(m.Content) == 0 {
		return nil
	}
	var blocks []primeContent
	if json.Unmarshal(m.Content, &blocks) == nil {
		return blocks
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil && s != "" {
		return []primeContent{{Type: "text", Text: s}}
	}
	return nil
}

// primeResultText flattens a tool_execution_end "result", which is an
// AgentToolResult — `{content: [{type:"text",text}...], details}` — including
// the error case, where the agent loop substitutes a single text block holding
// the failure message. A bare string is accepted too, in case the shape drifts.
func primeResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var r struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &r) == nil {
		return rawToText(r.Content)
	}
	return ""
}
