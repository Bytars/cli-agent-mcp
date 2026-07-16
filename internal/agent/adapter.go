// Package agent defines the pluggable adapter interface that lets the MCP
// server drive different headless CLI coding agents (Claude Code, Cursor, or any
// tool configured through CustomAdapter) through one uniform surface.
//
// Design: rather than trying to control an interactive TUI (pseudo-terminals,
// ANSI parsing, prompt detection — fragile), every adapter invokes its agent in
// *headless / print* mode, streaming newline-delimited output. That gives a
// clean programmatic contract: a session id to resume, and an unambiguous
// terminal "result" event signalling the task is done — with the process exit
// code as a universal backstop.
//
// To support another agent, implement Adapter and register it in the Registry.
package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunSpec fully describes one turn to run against an agent.
type RunSpec struct {
	Prompt    string   // the instruction/task text
	Cwd       string   // working directory for the child process
	Model     string   // optional model override
	SessionID string   // when set, resume this session instead of starting fresh
	ExtraArgs []string // extra flags appended verbatim
	PlanOnly  bool     // propose a plan without executing anything
}

// Event is the adapter's interpretation of a single line of agent stdout.
// Raw is always populated; the other fields are best-effort extractions.
type Event struct {
	Raw        string // the original line, always kept
	SessionID  string // non-empty when this line revealed the session id
	Text       string // human-facing text extracted from this line, if any
	Final      bool   // true when this is the terminal result event
	FinalError bool   // for a final event: whether the run reported an error
	FinalText  string // for a final event: the summarized result text
	ToolName   string // when the worker invoked a tool, its name (for the audit trail)
	ToolInput  string // the tool's input, compact JSON, truncated (for the audit trail)
}

// Adapter knows how to launch and interpret one specific CLI agent.
type Adapter interface {
	// Name is the stable identifier used in tool calls ("claude", "cursor").
	Name() string

	// Available reports whether the agent looks usable on this machine, with a
	// human-readable detail (resolved binary path or the reason it is missing).
	Available() (ok bool, detail string)

	// Command builds the exec.Cmd for a run. It must NOT set Dir or Env; the
	// task manager owns those. Env is inherited from the server process, so
	// whatever the host machine can reach (VPN routes, SSH agent, credentials)
	// is available to the child with no extra wiring.
	Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error)

	// ParseLine interprets one line of stdout.
	ParseLine(line string) Event
}

// ResultFromOutput is an optional interface for adapters whose agent has no
// reliable terminal result event. When an adapter implements it and returns
// true, the task manager falls back to the turn's collected output as the task
// result instead of leaving it empty.
type ResultFromOutput interface {
	UseOutputAsResult() bool
}

// PlanCapable is an optional interface for adapters whose agent can run a
// plan-only turn: propose the steps it *would* take, executing nothing.
//
// This must fail closed. Callers are required to check it before honouring
// RunSpec.PlanOnly, because an adapter that silently ignored PlanOnly would
// execute the task when the caller explicitly asked it not to.
type PlanCapable interface {
	SupportsPlanOnly() bool
}

// CanPlan reports whether a is able to honour RunSpec.PlanOnly.
func CanPlan(a Adapter) bool {
	p, ok := a.(PlanCapable)
	return ok && p.SupportsPlanOnly()
}

// Registry is the set of adapters known to the server.
type Registry struct {
	byName map[string]Adapter
	order  []string
}

// NewRegistry builds a registry from the given adapters, in order.
func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{byName: make(map[string]Adapter)}
	for _, a := range adapters {
		if a == nil {
			continue
		}
		if _, exists := r.byName[a.Name()]; !exists {
			r.order = append(r.order, a.Name())
		}
		r.byName[a.Name()] = a
	}
	return r
}

// Get returns the adapter for name, or nil.
func (r *Registry) Get(name string) Adapter {
	return r.byName[strings.ToLower(strings.TrimSpace(name))]
}

// Names returns the registered adapter names in registration order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// All returns adapters in registration order.
func (r *Registry) All() []Adapter {
	out := make([]Adapter, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// buildCommand creates an *exec.Cmd for bin+args, transparently handling
// Windows script wrappers so adapters can treat every launcher uniformly.
//
//   - .exe / no ext : executed directly (Go quotes args correctly).
//   - .ps1          : run via `powershell -NoProfile -ExecutionPolicy Bypass -File`.
//   - .cmd / .bat   : run via `cmd /c`.
//
// Prefer resolving to a real .exe where possible (see the Cursor adapter, which
// detects node.exe): direct execution avoids all cmd/powershell re-quoting.
func buildCommand(ctx context.Context, bin string, args []string) *exec.Cmd {
	resolved := bin
	if p, err := exec.LookPath(bin); err == nil {
		resolved = p
	}
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".ps1":
		psArgs := append([]string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", resolved}, args...)
		return exec.CommandContext(ctx, "powershell", psArgs...)
	case ".cmd", ".bat":
		cmdArgs := append([]string{"/c", resolved}, args...)
		return exec.CommandContext(ctx, "cmd", cmdArgs...)
	default:
		return exec.CommandContext(ctx, resolved, args...)
	}
}
