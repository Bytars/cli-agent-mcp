// SPDX-License-Identifier: Apache-2.0

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
	"strconv"
	"strings"
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
		// Month and day are NOT zero-padded (the pattern accepts \d{1,2}), so a
		// lexicographic sort ranks "2026.2.1" above "2026.10.1" and would pick a
		// stale install from October onwards. Compare the numeric components
		// instead, newest first, and fall back to string order for names that
		// don't parse. Take the newest directory that actually has the files.
		sort.Slice(versions, func(i, j int) bool {
			yi, mi, di, oki := parseCursorVersion(versions[i])
			yj, mj, dj, okj := parseCursorVersion(versions[j])
			if oki != okj {
				return oki // parseable names win over unparseable ones
			}
			if oki && okj {
				if yi != yj {
					return yi > yj
				}
				if mi != mj {
					return mi > mj
				}
				if di != dj {
					return di > dj
				}
			}
			// Same date (or neither parsed): descending string order, which also
			// makes the ordering total and therefore deterministic.
			return versions[i] > versions[j]
		})
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

// parseCursorVersion extracts the numeric year/month/day prefix of a
// cursor-agent version directory name (e.g. "2026.10.1-abc123" or
// "2026.10.1-01-02-03-abc123"). ok is false when the name does not have the
// expected three-component numeric head.
func parseCursorVersion(name string) (y, m, d int, ok bool) {
	head := name
	if i := strings.IndexByte(head, '-'); i >= 0 {
		head = head[:i]
	}
	parts := strings.Split(head, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
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
		return buildCommand(ctx, a.node, full)
	}
	return buildCommand(ctx, a.Bin, args)
}

// ParseLine is tolerant of Cursor's exact schema: it extracts a session id and a
// terminal result where recognizable, and always preserves the raw line so
// callers never lose output even if the schema drifts. Task completion is also
// backstopped by the process exit code in the task manager.
func (a *CursorAdapter) ParseLine(line string) Event {
	return parseTolerantLine(line, false)
}
