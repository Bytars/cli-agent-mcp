// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// KimiAdapter drives the Kimi Code CLI (`kimi`) in headless (`-p`) mode with
// streaming JSON.
//
// Kimi's `--output-format stream-json` schema (measured against kimi 2.0.2,
// win32-x64) is newline-delimited JSON with a `role` discriminator — NOT the
// Claude stream-json schema:
//
//	{"role":"meta","type":"system.version","version":"2.0.2"}
//	{"role":"assistant","content":"OK"}
//	{"role":"assistant","tool_calls":[{"type":"function","id":"…",
//	  "function":{"name":"Glob","arguments":"{\"pattern\":\"*.md\"}"}}]}
//	{"role":"tool","tool_call_id":"…","content":"…"}
//	{"role":"meta","type":"session.resume_hint",
//	  "session_id":"session_…","command":"kimi -r session_…","content":"…"}
//
// There is no terminal "result" event: the last assistant text is the answer
// and the process exit code is the backstop. The task manager's lastText
// fallback (the final human-facing text event) therefore yields exactly the
// worker's answer as the task result. Sessions resume with `--session <id>`
// (alias `-r`).
//
// Plan-only is NOT supported: kimi's `--plan` cannot be combined with `-p`, so
// there is no way to guarantee a nothing-executes turn. The adapter therefore
// does not implement PlanCapable and agent_plan_task fails closed for it.
type KimiAdapter struct {
	Bin       string   // launcher; defaults handled by caller ("kimi")
	ExtraArgs []string // appended verbatim
}

// NewKimiAdapter constructs a Kimi Code CLI adapter.
func NewKimiAdapter(bin string, extraArgs []string) *KimiAdapter {
	if bin == "" {
		bin = "kimi"
	}
	return &KimiAdapter{Bin: bin, ExtraArgs: extraArgs}
}

func (a *KimiAdapter) Name() string { return "kimi" }

func (a *KimiAdapter) Available() (bool, string) {
	if p, err := exec.LookPath(a.Bin); err == nil {
		return true, p
	}
	return false, fmt.Sprintf("%q not found in PATH (set CLI_AGENT_MCP_KIMI_BIN to its full path, e.g. %%USERPROFILE%%\\.kimi-code\\bin\\kimi.exe)", a.Bin)
}

func (a *KimiAdapter) Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error) {
	if spec.Prompt == "" {
		return nil, fmt.Errorf("kimi: empty prompt")
	}
	// -p            : print/headless mode (no TUI); non-interactive runs use
	//                 auto permissions by default (measured: tool calls execute
	//                 without prompting)
	// stream-json   : one JSON object per line (role-discriminated schema)
	args := []string{
		"-p", spec.Prompt,
		"--output-format", "stream-json",
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.SessionID != "" {
		// Continue an existing conversation, applying the new prompt as the turn.
		args = append(args, "--session", spec.SessionID)
	}
	args = append(args, a.ExtraArgs...)
	args = append(args, spec.ExtraArgs...)
	return buildCommand(ctx, a.Bin, args)
}

// kimiStreamEvent mirrors the subset of Kimi's stream-json schema we care
// about. Unknown fields and event types are ignored (Raw is always kept).
type kimiStreamEvent struct {
	Role      string `json:"role"`
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	// assistant/tool payloads: content is a string in every event measured so
	// far, but decode tolerantly in case a future version emits blocks.
	Content   json.RawMessage `json:"content"`
	ToolCalls []struct {
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	IsError bool `json:"is_error"`
}

func (a *KimiAdapter) ParseLine(line string) Event {
	ev := Event{Raw: line}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		// Non-JSON chatter: surface as plain progress text.
		ev.Text = trimmed
		return ev
	}

	var se kimiStreamEvent
	if err := json.Unmarshal([]byte(trimmed), &se); err != nil {
		return ev
	}

	if se.SessionID != "" {
		ev.SessionID = se.SessionID
	}

	switch se.Role {
	case "assistant":
		var b strings.Builder
		if text := rawToText(se.Content); text != "" {
			appendPart(&b, text)
		}
		for _, tc := range se.ToolCalls {
			if tc.Function.Name == "" {
				continue
			}
			appendPart(&b, "⚙ using "+tc.Function.Name)
			// Capture structured tool use for the audit trail (keep the last
			// one if several are in a message). Kimi sends arguments as a
			// JSON-encoded string, so unwrap it before truncating.
			ev.ToolName = tc.Function.Name
			ev.ToolInput = truncateJSON(unwrapJSONString(tc.Function.Arguments), 400)
		}
		ev.Text = b.String()
	case "tool":
		ev.IsToolResult = true
		if se.IsError {
			ev.ToolResultError = true
		}
		last := lastNonEmptyLine(rawToText(se.Content))
		if last == "" {
			if se.IsError {
				last = "(failed with no output — possibly blocked by security software / sandbox)"
			} else {
				return ev
			}
		}
		prefix := "↳ "
		if se.IsError {
			prefix = "↳ ✗ "
		}
		ev.Text = prefix + last
	case "meta":
		// system.version / session.resume_hint and friends: session id already
		// captured above; the rest is noise for the transcript. Error metas are
		// the exception — surface them.
		if strings.Contains(se.Type, "error") {
			if text := rawToText(se.Content); text != "" {
				ev.Text = "✗ " + text
			}
		}
	default:
		// Unknown roles with text content: show rather than drop.
		if text := rawToText(se.Content); text != "" {
			ev.Text = text
		}
	}
	return ev
}

// unwrapJSONString handles fields that arrive as a JSON-encoded string of JSON
// (Kimi's tool_call "arguments"): if raw is a JSON string, return its contents;
// otherwise return raw unchanged.
func unwrapJSONString(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return json.RawMessage(s)
	}
	return raw
}
