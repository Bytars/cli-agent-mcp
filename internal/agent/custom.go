package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CustomAdapter drives *any* CLI agent, configured entirely from the
// environment — no Go code required. This is the escape hatch that makes the
// server usable with tools this project doesn't ship an adapter for.
//
// You give it a binary and an argument template. Each template argument may
// contain placeholders that are substituted per run:
//
//	{{prompt}}   the task text
//	{{cwd}}      the working directory
//	{{model}}    the model override (may be empty)
//	{{session}}  the session id when resuming (empty on the first turn)
//
// Rule: if an argument's placeholder expands to an empty value, that whole
// argument is dropped. So write optional flags in the single-argument
// `--flag=value` form (e.g. `--model={{model}}`) — that way the flag disappears
// cleanly when the value is absent, instead of leaving a dangling `--model`.
//
// Output handling is deliberately forgiving: JSON lines are parsed tolerantly
// (a recognized session id enables follow-ups; a recognized terminal event sets
// the result), and plain-text lines are streamed as progress. Completion is
// determined by the process exit code, and because most simple CLIs just print
// their answer, the collected output is used as the task result.
type CustomAdapter struct {
	name         string
	Bin          string
	ArgsTemplate []string
}

// NewCustomAdapter builds a user-configured adapter. An empty name defaults to
// "custom"; an empty bin yields an adapter that reports itself unconfigured.
func NewCustomAdapter(name, bin string, argsTemplate []string) *CustomAdapter {
	if strings.TrimSpace(name) == "" {
		name = "custom"
	}
	return &CustomAdapter{name: name, Bin: bin, ArgsTemplate: argsTemplate}
}

func (a *CustomAdapter) Name() string { return a.name }

func (a *CustomAdapter) Available() (bool, string) {
	if strings.TrimSpace(a.Bin) == "" {
		return false, "not configured (set CLI_AGENT_MCP_CUSTOM_BIN and CLI_AGENT_MCP_CUSTOM_ARGS)"
	}
	if p, err := exec.LookPath(a.Bin); err == nil {
		return true, p + " " + strings.Join(a.ArgsTemplate, " ")
	}
	return false, fmt.Sprintf("%q not found in PATH", a.Bin)
}

// UseOutputAsResult reports that this agent has no reliable terminal result
// event, so the task manager should fall back to its collected output.
func (a *CustomAdapter) UseOutputAsResult() bool { return true }

func (a *CustomAdapter) Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error) {
	if strings.TrimSpace(a.Bin) == "" {
		return nil, fmt.Errorf("custom agent is not configured: set CLI_AGENT_MCP_CUSTOM_BIN")
	}
	if spec.Prompt == "" {
		return nil, fmt.Errorf("%s: empty prompt", a.name)
	}
	vals := map[string]string{
		"prompt":  spec.Prompt,
		"cwd":     spec.Cwd,
		"model":   spec.Model,
		"session": spec.SessionID,
	}
	args := make([]string, 0, len(a.ArgsTemplate)+len(spec.ExtraArgs))
	for _, tmpl := range a.ArgsTemplate {
		if arg, keep := expandArg(tmpl, vals); keep {
			args = append(args, arg)
		}
	}
	args = append(args, spec.ExtraArgs...)
	return buildCommand(ctx, a.Bin, args), nil
}

func (a *CustomAdapter) ParseLine(line string) Event {
	return parseTolerantLine(line, true)
}

// expandArg substitutes {{...}} placeholders in a template argument. It reports
// keep=false when a referenced placeholder is empty, meaning the caller should
// drop the argument entirely.
func expandArg(tmpl string, vals map[string]string) (string, bool) {
	out := tmpl
	for k, v := range vals {
		ph := "{{" + k + "}}"
		if !strings.Contains(out, ph) {
			continue
		}
		if v == "" {
			return "", false
		}
		out = strings.ReplaceAll(out, ph, v)
	}
	return out, true
}
