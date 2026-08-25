// SPDX-License-Identifier: Apache-2.0

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
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunSpec fully describes one turn to run against an agent.
type RunSpec struct {
	Prompt       string   // the instruction/task text
	Cwd          string   // working directory for the child process
	Model        string   // optional model override
	SessionID    string   // when set, resume this session instead of starting fresh
	ExtraArgs    []string // extra flags appended verbatim
	AllowedTools []string // per-run tools to pre-approve (merged with server policy)
	PlanOnly     bool     // propose a plan without executing anything

	// MCPConfigPath and PermissionTool wire the agent to an approval endpoint,
	// so a tool call it would otherwise stall on becomes a question asked of
	// the human upstream. Both are set together or not at all.
	MCPConfigPath  string
	PermissionTool string
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

	IsToolResult    bool // this line is a tool's result coming back
	ToolResultError bool // for a tool result: whether the tool reported failure
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
		// Look-ups normalise the name, so registration must too — otherwise an
		// adapter named "Aider" is listed as available but can never be reached.
		key := normalizeName(a.Name())
		if key == "" {
			log.Printf("cli-agent-mcp: ignoring adapter with an empty name")
			continue
		}
		if prev, exists := r.byName[key]; exists {
			// Silently replacing a built-in would drop its whole policy
			// (permission mode, disallowed tools) with no trace.
			log.Printf("cli-agent-mcp: adapter name %q is already registered by %T; keeping the first and ignoring %T", key, prev, a)
			continue
		}
		r.order = append(r.order, key)
		r.byName[key] = a
	}
	return r
}

// Get returns the adapter for name, or nil.
func (r *Registry) Get(name string) Adapter {
	return r.byName[normalizeName(name)]
}

// normalizeName is the single definition of adapter-name identity, used by both
// registration and look-up so the two can never disagree.
func normalizeName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

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

// buildCommand creates an *exec.Cmd for bin+args, resolving Windows script
// wrappers so adapters can treat every launcher uniformly.
//
//   - script shim    : resolved to whatever it would have run (`node.exe
//     entry.js ...`, or a bundled `.exe`) and run directly.
//   - .exe / no ext  : executed directly (Go quotes args correctly).
//   - .ps1 / .cmd    : last resort, via an interpreter anchored to System32, and
//     only when no argument contains a character that a second parser could
//     reinterpret. Otherwise it returns an actionable error.
//
// Direct execution is always preferred: it keeps argv verbatim, avoids every
// shell-quoting hazard, and removes an interpreter hop that packaged hosts
// handle poorly.
func buildCommand(ctx context.Context, bin string, args []string) (*exec.Cmd, error) {
	resolved := bin
	if p, err := exec.LookPath(bin); err == nil {
		resolved = p
	}

	// Preferred path: if this is a script shim, run what it would have run. One
	// process, argv passed verbatim, no shell parser involved.
	if exe, prefix, ok := resolveScriptShim(resolved); ok {
		full := append(append([]string(nil), prefix...), args...)
		return hardenSpawn(exec.CommandContext(ctx, exe, full...)), nil
	}

	// Second chance before the interpreter: PATH order decides which of several
	// installs LookPath returns, and it can hand back a shim while a native
	// executable for the same command sits further down. Running that directly
	// is strictly better than shelling out to a wrapper we could not read.
	if native := nativeAlternative(bin, resolved); native != "" {
		return hardenSpawn(exec.CommandContext(ctx, native, args...)), nil
	}

	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".ps1":
		if bad := unsafeForShell(args); bad != "" {
			return nil, shellArgError(resolved, bad, "PowerShell")
		}
		psArgs := append([]string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", resolved}, args...)
		return hardenSpawn(exec.CommandContext(ctx, powershellPath(), psArgs...)), nil
	case ".cmd", ".bat":
		if bad := unsafeForShell(args); bad != "" {
			return nil, shellArgError(resolved, bad, "cmd.exe")
		}
		cmdArgs := append([]string{"/c", resolved}, args...)
		return hardenSpawn(exec.CommandContext(ctx, cmdPath(), cmdArgs...)), nil
	default:
		return hardenSpawn(exec.CommandContext(ctx, resolved, args...)), nil
	}
}

// nativeExts are the extensions of things Windows can execute without an
// interpreter, in the order we would rather have them.
var nativeExts = []string{".exe", ".com"}

// nativeAlternative looks for a real executable that answers to the same command
// as a script shim we could not read. It returns "" unless resolved really is a
// shim and a native sibling exists, so on every other platform and every other
// launcher it is a no-op.
//
// It searches the shim's own directory first — an install that ships both keeps
// them together — then the PATH, which is where a second, independent install of
// the same agent turns up.
func nativeAlternative(bin, resolved string) string {
	switch strings.ToLower(filepath.Ext(resolved)) {
	case ".cmd", ".bat", ".ps1":
	default:
		return ""
	}

	stem := strings.TrimSuffix(filepath.Base(resolved), filepath.Ext(resolved))
	dirs := []string{filepath.Dir(resolved)}
	dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, ext := range nativeExts {
			cand := filepath.Join(dir, stem+ext)
			if fileExists(cand) {
				log.Printf("cli-agent-mcp: %q resolved to the script shim %s, which could not be read; using %s instead", bin, resolved, cand)
				return cand
			}
		}
	}
	return ""
}

// shellMetaChars are the characters we cannot round-trip safely through a
// second parser. `"` terminates cmd.exe's quoting (it does not understand Go's
// `\"`), and `%` / `!` are expanded even inside quotes.
const shellMetaChars = "\"%!"

// unsafeForShell returns the first argument that cannot be passed through an
// interpreter without risk, or "" when all are safe.
func unsafeForShell(args []string) string {
	for _, a := range args {
		if strings.ContainsAny(a, shellMetaChars) {
			return a
		}
	}
	return ""
}

func shellArgError(launcher, arg, interp string) error {
	preview := arg
	if len(preview) > 80 {
		preview = preview[:80] + "…"
	}
	return fmt.Errorf(
		"refusing to run %s through %s: an argument contains characters that cannot be quoted safely (%q).\n"+
			"This launcher is a script shim and its Node entry point could not be resolved automatically.\n"+
			"Point the agent at a real executable instead — e.g. set the *_BIN variable to node.exe or to the agent's .exe.\n"+
			"Offending argument: %s",
		filepath.Base(launcher), interp, shellMetaChars, preview)
}

// cmdPath and powershellPath anchor the interpreters to the system directory.
// Resolving them through PATH invites hijacking, and under a packaged host the
// PATH may contain app-execution aliases that behave differently from the real
// binaries.
func cmdPath() string {
	if root := systemRoot(); root != "" {
		if p := filepath.Join(root, "System32", "cmd.exe"); fileExists(p) {
			return p
		}
	}
	return "cmd"
}

func powershellPath() string {
	if root := systemRoot(); root != "" {
		p := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if fileExists(p) {
			return p
		}
	}
	return "powershell"
}
