// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/winspawn"
)

// rig is the scaffolding every mode borrows: the built server, a set of
// stand-in launchers planted at telling paths, and the scratch state each mode
// starts from.
type rig struct {
	// root is the module directory, so the harness builds the tree it lives in
	// rather than whatever happens to be on PATH.
	root string

	// dir holds everything this run creates and is removed at the end. It is
	// under the OS temp directory and NOTHING here ever touches the real
	// %APPDATA%\cli-agent-mcp or %LOCALAPPDATA%\Packages — see env, which fences
	// both off for every child process.
	dir string

	// server is the binary under test: the real one, built from source, not a
	// package imported into this process. A pairing verdict is reached from a
	// parent-process lookup and a file on disk, and neither exists for a
	// function call.
	server string

	// The planted launchers. claudeV0 and claudeV1 are the SAME MSIX package at
	// two versions — the background update that started issue #29 — and other
	// is a different package wearing the same executable name, which is the
	// control that stops "claude.exe" from being what authorizes anything.
	claudeV0 string
	claudeV1 string
	other    string
	editor   string

	passed   int
	failed   int
	failures []string
}

// newRig builds the binaries and plants the launchers.
func newRig(root string) (*rig, error) {
	dir, err := os.MkdirTemp("", "cli-agent-mcp-pairing-e2e-")
	if err != nil {
		return nil, err
	}
	r := &rig{root: root, dir: dir}

	r.server = filepath.Join(dir, exeName("cli-agent-mcp"))
	if err := r.goBuild(r.server, "."); err != nil {
		return nil, err
	}
	stand := filepath.Join(dir, exeName("launcher"))
	if err := r.goBuild(stand, "./cmd/pairing-e2e/launcher"); err != nil {
		return nil, err
	}

	// MSIX identity is simulated purely by the SHAPE OF THE PATH, because that
	// is all IdentityOf reads: a directory segment like
	// Claude_1.40609.0.0_x64__pzs8sxrjxfjjc is what makes a launcher "the app
	// package Claude_pzs8sxrjxfjjc". No package needs to be installed, and none
	// should be — the real WindowsApps tree is not writable and the live install
	// is not this harness's to disturb.
	apps := filepath.Join(dir, "WindowsApps")
	r.claudeV0 = filepath.Join(apps, "Claude_1.40609.0.0_x64__pzs8sxrjxfjjc", "app", exeName("claude"))
	r.claudeV1 = filepath.Join(apps, "Claude_1.40609.1.0_x64__pzs8sxrjxfjjc", "app", exeName("claude"))
	r.other = filepath.Join(apps, "Otra_1.0.0.0_x64__zzzzzzzzzzzzz", "app", exeName("claude"))
	r.editor = filepath.Join(dir, "Editor", exeName("editor"))
	for _, p := range []string{r.claudeV0, r.claudeV1, r.other, r.editor} {
		if err := plant(stand, p); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *rig) cleanup() {
	_ = os.RemoveAll(r.dir)
}

// goBuild compiles pkg out of the module this harness lives in.
func (r *rig) goBuild(out, pkg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := winspawn.Harden(exec.CommandContext(ctx, "go", "build", "-o", out, pkg))
	cmd.Dir = r.root
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %v\n%s", pkg, err, b)
	}
	return nil
}

// plant copies the stand-in launcher to one more path. The copy is the point:
// pairing compares WHERE a program lives, so the same bytes at four paths are
// four different clients as far as the server is concerned.
func plant(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o700)
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// env builds the environment for a child process.
//
// THE THREE VARIABLES IT STRIPS ARE THE SAFETY PROPERTY OF THIS WHOLE PROGRAM.
// This machine has a live install. CLI_AGENT_MCP_STATE_DIR decides which
// pairing record the server reads; APPDATA is what os.UserConfigDir answers
// with when that is unset, so it decides the same thing by another route; and
// LOCALAPPDATA is where the MSIX mirror is looked for. Inheriting any of the
// three would let a mode with a missing argument quietly rewrite — or unpair —
// the developer's real client. All three are re-pointed inside the scratch
// directory, so the worst a bug in here can do is corrupt its own temp tree.
// A mode may override any of them through extra — mode "mirror" has to, since
// the whole subject there is where LOCALAPPDATA points — but it overrides them
// with another scratch path, never with the inherited one.
//
// The set is resolved into a map before the slice is built rather than appended
// after it. Appending would work, because os/exec keeps the last value for a
// repeated key, but resting a safety property on a dedup rule is how it stops
// holding the day that rule is not what someone assumed.
func (r *rig) env(stateDir string, extra ...string) []string {
	set := map[string]string{
		// A LOCALAPPDATA with no Packages tree under it: packagedRecords finds
		// nothing, so no mode except the one about the mirror ever sees a
		// warning about it.
		"APPDATA":      filepath.Join(r.dir, "Roaming"),
		"LOCALAPPDATA": filepath.Join(r.dir, "Local"),
	}
	if stateDir != "" {
		set["CLI_AGENT_MCP_STATE_DIR"] = stateDir
	}
	for _, kv := range extra {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set[strings.ToUpper(k)] = v
		}
	}
	out := make([]string, 0, len(os.Environ())+len(set))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		ku := strings.ToUpper(k)
		// An empty stateDir means "let the command's own --state-dir decide", so
		// the variable is dropped rather than set. Dropping is not optional: an
		// inherited one would silently outrank the flag under test.
		if _, replaced := set[ku]; replaced || ku == "CLI_AGENT_MCP_STATE_DIR" {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range set {
		out = append(out, k+"="+v)
	}
	return out
}

// run executes exe and returns everything it wrote to stdout and stderr.
//
// The exit code is deliberately not asserted on. A refusing server exits
// non-zero and so does `pair --status` on an unpaired record; what is being
// measured is what the user is TOLD, which is the thing every one of these six
// failure modes got wrong. A start-up failure is folded into the text so an
// unreadable check reports why rather than mismatching against nothing.
func (r *rig) run(env []string, exe string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := winspawn.Harden(exec.CommandContext(ctx, exe, args...))
	cmd.Env = env
	cmd.Dir = r.dir
	b, err := cmd.CombinedOutput()
	out := string(b)
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		out += "\n[harness: " + err.Error() + "]"
	}
	return out
}

// startAll starts the server THROUGH a planted launcher and returns everything
// the start printed.
//
// The launcher is what makes this an end-to-end proof: run the server directly
// and its parent is this harness, one identity, forever — which is precisely
// the thing pairing keys on and therefore the thing that has to vary.
func (r *rig) startAll(stateDir, launcher string) string {
	return r.run(r.env(stateDir), launcher, r.server)
}

// startLine returns just the server's verdict about itself: the first line it
// logs under its own prefix, with the prefix removed.
//
// Not always the first line of output, which is why startAll exists too — an
// unreadable record makes the server log the read error first, and mode 4 has
// to see both.
func (r *rig) startLine(stateDir, launcher string) string {
	const prefix = "cli-agent-mcp: "
	for _, line := range strings.Split(r.startAll(stateDir, launcher), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

// pair runs a `cli-agent-mcp pair` command the way a person at a terminal would.
func (r *rig) pair(stateDir string, args ...string) string {
	return r.run(r.env(stateDir), r.server, append([]string{"pair"}, args...)...)
}

// freshState wipes and recreates a scratch state directory, and returns it.
//
// Every mode starts from one. Pairing state is cumulative — a trusted launcher,
// a warning already issued — so a mode that inherited the previous mode's
// record would be asserting about a history nobody wrote down.
func (r *rig) freshState(name string) string {
	dir := filepath.Join(r.dir, "state-"+name)
	if err := os.RemoveAll(dir); err != nil {
		panic("pairing-e2e: " + err.Error())
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic("pairing-e2e: " + err.Error())
	}
	return dir
}

func recordPath(stateDir string) string {
	return filepath.Join(stateDir, "pairing.json")
}

// age backdates every trusted launcher so the record's learning window has
// closed.
//
// Without this most of the modes measure nothing. A fresh record spends its
// first 24 hours ADOPTING whatever launches the server (launcher.go's
// learningWindow), so "a stranger is refused" and "a stranger is welcomed"
// produce the same green run on a record minutes old. Backdating is what turns
// the window off and lets the judging behaviour be seen at all.
//
// It edits the JSON as plain maps rather than through pairing.File on purpose.
// Round-tripping the record through the very type under test would rewrite
// every field the way that type currently spells it, so a change in the on-disk
// format would keep this harness passing while every real installed record
// stopped being understood.
func age(stateDir string) error {
	p := recordPath(stateDir)
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	list, _ := doc["trusted_launchers"].([]any)
	if len(list) == 0 {
		return fmt.Errorf("%s has no trusted_launchers to age; the record is not what this mode assumed", p)
	}
	for _, item := range list {
		if l, ok := item.(map[string]any); ok {
			l["recorded"] = "2020-01-01T00:00:00Z"
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(p, out, 0o600)
}

// matches applies one expectation, case-insensitively and as an alternation,
// exactly as the hand-run proof this is a port of did.
func matches(pattern, got string) bool {
	return regexp.MustCompile("(?i)" + pattern).MatchString(got)
}

// moduleRoot walks up from the working directory to the module this harness
// belongs to.
//
// It builds the tree it is IN rather than trusting a `cli-agent-mcp` on PATH.
// A harness that silently tested the installed copy would report on a binary
// nobody changed, which is the same class of unfalsifiable result issue #29 was
// made of.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), "module github.com/Bytars/cli-agent-mcp") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no cli-agent-mcp go.mod above the working directory; run this from inside the repository")
		}
		dir = parent
	}
}
