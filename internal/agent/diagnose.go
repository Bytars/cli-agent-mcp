package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Diagnose answers the question that is otherwise very expensive to answer from
// the outside: "can this process actually start a child, and what would each
// agent run?"
//
// It exists because of a specific, hard-won failure mode. When this server is
// launched by an MSIX-packaged host (Claude Desktop on Windows is packaged),
// child processes inherit package identity, and some binaries die during
// start-up without writing a single byte to stdout or stderr — the caller sees
// only a non-zero exit code and no explanation. Guessing at that from the
// symptoms costs hours. Measuring it costs one tool call.

// SpawnProbe is the result of trying to run one harmless command.
type SpawnProbe struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
	Err      string `json:"error,omitempty"`
	Silent   bool   `json:"silent,omitempty"` // failed AND produced no output at all
}

// AdapterDiag reports how one adapter resolves on this machine.
type AdapterDiag struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail"`
	Launcher  string `json:"launcher,omitempty"` // what PATH resolution found
	RunsAs    string `json:"runs_as,omitempty"`  // what we would actually execute
	ShimFixed bool   `json:"shim_resolved,omitempty"`
}

// DiagnosticReport is the full picture.
type DiagnosticReport struct {
	OS            string        `json:"os"`
	Arch          string        `json:"arch"`
	Executable    string        `json:"executable"`
	Packaged      bool          `json:"packaged_app"`
	PackageName   string        `json:"package_name,omitempty"`
	SpawnProbes   []SpawnProbe  `json:"spawn_probes"`
	Adapters      []AdapterDiag `json:"adapters"`
	Notes         []string      `json:"notes,omitempty"`
	SpawnWorks    bool          `json:"spawn_works"`
	SilentFailure bool          `json:"silent_failure_detected"`
}

// Diagnose runs the probes. It never mutates anything and never runs a
// user-supplied command.
func Diagnose(ctx context.Context, reg *Registry) DiagnosticReport {
	rep := DiagnosticReport{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if exe, err := os.Executable(); err == nil {
		rep.Executable = exe
	}
	rep.Packaged, rep.PackageName = packageIdentity()

	for _, p := range probeCommands() {
		rep.SpawnProbes = append(rep.SpawnProbes, runProbe(ctx, p.name, p.bin, p.args))
	}

	okCount, silent := 0, false
	for _, p := range rep.SpawnProbes {
		if p.OK {
			okCount++
		}
		if p.Silent {
			silent = true
		}
	}
	rep.SpawnWorks = okCount > 0
	rep.SilentFailure = silent

	if reg != nil {
		for _, a := range reg.All() {
			d := AdapterDiag{Name: a.Name()}
			d.Available, d.Detail = a.Available()
			if b := binOf(a); b != "" {
				if p, err := exec.LookPath(b); err == nil {
					d.Launcher = p
					if node, entry, ok := resolveScriptShim(p); ok {
						d.RunsAs = node + " " + entry
						d.ShimFixed = true
					} else {
						d.RunsAs = p
					}
				} else {
					d.Launcher = b + " (not found in PATH)"
				}
			}
			rep.Adapters = append(rep.Adapters, d)
		}
	}

	rep.Notes = buildNotes(rep)
	return rep
}

// binOf extracts the configured launcher from the concrete adapter types so the
// report can show what PATH resolution produced.
func binOf(a Adapter) string {
	switch v := a.(type) {
	case *ClaudeAdapter:
		return v.Bin
	case *CursorAdapter:
		return v.Bin
	case *CustomAdapter:
		return v.Bin
	}
	return ""
}

type probeSpec struct {
	name string
	bin  string
	args []string
}

func probeCommands() []probeSpec {
	if runtime.GOOS == "windows" {
		return []probeSpec{
			{"system interpreter", cmdPath(), []string{"/c", "ver"}},
			{"node on PATH", "node", []string{"-v"}},
		}
	}
	return []probeSpec{
		{"system shell", "/bin/sh", []string{"-c", "echo ok"}},
		{"node on PATH", "node", []string{"-v"}},
	}
}

func runProbe(ctx context.Context, name, bin string, args []string) SpawnProbe {
	p := SpawnProbe{Name: name, Command: bin + " " + strings.Join(args, " ")}

	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := hardenSpawn(exec.CommandContext(cctx, bin, args...))
	out, err := cmd.CombinedOutput()
	p.Output = strings.TrimSpace(string(out))
	if len(p.Output) > 300 {
		p.Output = p.Output[:300] + "…"
	}
	if err != nil {
		p.Err = err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			p.ExitCode = ee.ExitCode()
		} else {
			p.ExitCode = -1
		}
		// The signature of the packaged-host failure: a real exit code with
		// nothing written to either stream.
		p.Silent = p.ExitCode > 0 && p.Output == ""
		return p
	}
	p.OK = true
	return p
}

func buildNotes(r DiagnosticReport) []string {
	var n []string
	if r.Packaged {
		n = append(n, "This process runs with package identity ("+r.PackageName+"). Child processes inherit it, along with its filesystem virtualization and its console-less environment.")
	}
	if !r.SpawnWorks {
		n = append(n, "NO spawn probe succeeded: launching child processes does not work in this context. A CLI agent cannot operate this way. Run the server from an unpackaged host (for example the Claude Code CLI in a terminal).")
	}
	if r.SilentFailure {
		n = append(n, "At least one probe failed with an exit code and NO output on stdout or stderr. That is the exact symptom of a startup aborted under packaging: the binary dies before it can report the cause.")
	}
	for _, a := range r.Adapters {
		if a.ShimFixed {
			n = append(n, "Adapter "+a.Name+": the launcher is a script shim and was resolved to its real runtime. It runs directly, with no intermediate interpreter.")
		}
	}
	if r.SpawnWorks && !r.SilentFailure {
		n = append(n, "Child process spawning works normally in this context.")
	}
	return n
}

// Text renders the report for a human reading the tool result.
func (r DiagnosticReport) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== Environment ===\n")
	fmt.Fprintf(&b, "os/arch    : %s/%s\n", r.OS, r.Arch)
	fmt.Fprintf(&b, "executable : %s\n", r.Executable)
	if r.Packaged {
		fmt.Fprintf(&b, "packaged   : YES — %s\n", r.PackageName)
	} else {
		fmt.Fprintf(&b, "packaged   : no\n")
	}

	fmt.Fprintf(&b, "\n=== Spawn probes ===\n")
	for _, p := range r.SpawnProbes {
		status := "OK"
		if !p.OK {
			status = fmt.Sprintf("FAILED exit=%d", p.ExitCode)
			if p.Silent {
				status += " NO OUTPUT"
			}
		}
		fmt.Fprintf(&b, "  %-24s %-22s %s\n", p.Name, status, firstLine(p.Output, p.Err))
	}

	fmt.Fprintf(&b, "\n=== Agents ===\n")
	for _, a := range r.Adapters {
		avail := "unavailable"
		if a.Available {
			avail = "available"
		}
		fmt.Fprintf(&b, "  %-10s %-14s %s\n", a.Name, avail, a.Detail)
		if a.RunsAs != "" {
			mark := ""
			if a.ShimFixed {
				mark = "  (shim resolved)"
			}
			fmt.Fprintf(&b, "  %-10s runs as: %s%s\n", "", a.RunsAs, mark)
		}
	}

	if len(r.Notes) > 0 {
		fmt.Fprintf(&b, "\n=== Notes ===\n")
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
	}
	return b.String()
}

func firstLine(out, errStr string) string {
	s := out
	if s == "" {
		s = errStr
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return s
}
