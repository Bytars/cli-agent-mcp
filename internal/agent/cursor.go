package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
)

// CursorAdapter drives Cursor's headless agent (`cursor-agent -p`).
//
// The `cursor-agent` launcher is a shell/PowerShell wrapper around a bundled
// Node runtime. Where possible we detect that bundled node + index.js and invoke
// them directly, mirroring the wrapper's own dispatch logic: on Windows this
// avoids .cmd/.ps1 re-quoting entirely. If detection fails we simply fall back to
// the `cursor-agent` launcher on PATH.
type CursorAdapter struct {
	Bin       string   // fallback launcher when detection fails ("cursor-agent")
	ExtraArgs []string // appended verbatim

	node  string // detected node binary (empty if not found)
	entry string // detected index.js
}

var cursorVersionRE = regexp.MustCompile(`^\d{4}\.\d{1,2}\.\d{1,2}(-\d{2}-\d{2}-\d{2})?-[a-f0-9]+$`)

// NewCursorAdapter constructs a Cursor adapter, attempting node.exe detection.
func NewCursorAdapter(bin string, extraArgs []string) *CursorAdapter {
	if bin == "" {
		bin = "cursor-agent"
	}
	a := &CursorAdapter{Bin: bin, ExtraArgs: extraArgs}
	a.node, a.entry = detectCursorNode()
	return a
}

// cursorVersionsDirs returns the candidate install roots for cursor-agent across
// platforms, most likely first.
func cursorVersionsDirs() []string {
	var dirs []string
	if la := os.Getenv("LOCALAPPDATA"); la != "" { // Windows
		dirs = append(dirs, filepath.Join(la, "cursor-agent", "versions"))
	}
	if home, err := os.UserHomeDir(); err == nil { // macOS / Linux
		dirs = append(dirs,
			filepath.Join(home, ".local", "share", "cursor-agent", "versions"),
			filepath.Join(home, ".cursor-agent", "versions"),
		)
	}
	return dirs
}

// nodeBinaryName is the bundled Node executable's name for this platform.
func nodeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "node.exe"
	}
	return "node"
}

// detectCursorNode replicates cursor-agent's launcher: it finds the install root,
// picks the newest version directory, and returns its node binary + index.js.
func detectCursorNode() (node, entry string) {
	for _, versionsDir := range cursorVersionsDirs() {
		entries, err := os.ReadDir(versionsDir)
		if err != nil {
			continue
		}
		var versions []string
		for _, e := range entries {
			if e.IsDir() && cursorVersionRE.MatchString(e.Name()) {
				versions = append(versions, e.Name())
			}
		}
		if len(versions) == 0 {
			continue
		}
		// The zero-padded date form sorts correctly lexicographically; take the
		// newest directory that actually contains the files.
		sort.Sort(sort.Reverse(sort.StringSlice(versions)))
		for _, v := range versions {
			n := filepath.Join(versionsDir, v, nodeBinaryName())
			idx := filepath.Join(versionsDir, v, "index.js")
			if fileExists(n) && fileExists(idx) {
				return n, idx
			}
		}
	}
	return "", ""
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func (a *CursorAdapter) Name() string { return "cursor" }

func (a *CursorAdapter) Available() (bool, string) {
	if a.node != "" {
		return true, a.node + " " + a.entry
	}
	// No bundled runtime found — the launcher on PATH works just as well.
	if p, err := exec.LookPath(a.Bin); err == nil {
		return true, p
	}
	return false, fmt.Sprintf("%q not found in PATH and no bundled runtime detected (set CLI_AGENT_MCP_CURSOR_BIN to its full path)", a.Bin)
}

func (a *CursorAdapter) Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error) {
	if spec.Prompt == "" {
		return nil, fmt.Errorf("cursor: empty prompt")
	}
	args := []string{
		"-p", spec.Prompt,
		"--output-format", "stream-json",
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.SessionID != "" {
		args = append(args, "--resume", spec.SessionID)
	}
	args = append(args, a.ExtraArgs...)
	args = append(args, spec.ExtraArgs...)

	if a.node != "" {
		// node.exe index.js <args...> — a clean .exe invocation, no wrapper.
		full := append([]string{a.entry}, args...)
		return buildCommand(ctx, a.node, full), nil
	}
	return buildCommand(ctx, a.Bin, args), nil
}

// ParseLine is tolerant of Cursor's exact schema: it extracts a session id and a
// terminal result where recognizable, and always preserves the raw line so
// callers never lose output even if the schema drifts. Task completion is also
// backstopped by the process exit code in the task manager.
func (a *CursorAdapter) ParseLine(line string) Event {
	return parseTolerantLine(line, false)
}
