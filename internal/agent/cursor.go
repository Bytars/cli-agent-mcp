package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// CursorAdapter drives Cursor's headless agent (`cursor-agent -p`).
//
// On Windows the `cursor-agent` launcher is a .ps1/.cmd wrapper around a bundled
// Node runtime. To avoid Windows script re-quoting we detect the bundled
// node.exe + index.js and invoke them directly, mirroring the wrapper's own
// dispatch logic.
type CursorAdapter struct {
	Bin       string   // fallback launcher when detection fails ("cursor-agent")
	ExtraArgs []string // appended verbatim

	node  string // detected node.exe (empty if not found)
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

// detectCursorNode replicates cursor-agent's launcher: under
// %LOCALAPPDATA%\cursor-agent\versions it picks the newest version directory and
// returns its node.exe + index.js.
func detectCursorNode() (node, entry string) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", ""
	}
	versionsDir := filepath.Join(base, "cursor-agent", "versions")
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		return "", ""
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && cursorVersionRE.MatchString(e.Name()) {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return "", ""
	}
	// Newest last lexicographically works for the zero-padded date form; sort
	// descending and take the first that actually contains the files.
	sort.Sort(sort.Reverse(sort.StringSlice(versions)))
	for _, v := range versions {
		n := filepath.Join(versionsDir, v, "node.exe")
		idx := filepath.Join(versionsDir, v, "index.js")
		if fileExists(n) && fileExists(idx) {
			return n, idx
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
	return false, fmt.Sprintf("bundled node.exe not detected; will fall back to %q launcher", a.Bin)
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
	ev := Event{Raw: line}
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return ev
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return ev
	}

	// Session id under any of the common key spellings.
	for _, k := range []string{"session_id", "sessionId", "chatId", "chat_id", "thread_id", "threadId"} {
		if s, ok := m[k].(string); ok && s != "" {
			ev.SessionID = s
			break
		}
	}

	typ, _ := m["type"].(string)
	switch typ {
	case "result", "final", "done", "complete":
		ev.Final = true
		if b, ok := m["is_error"].(bool); ok {
			ev.FinalError = b
		}
		if s, ok := m["result"].(string); ok {
			ev.FinalText = s
		} else if s, ok := m["text"].(string); ok {
			ev.FinalText = s
		}
	case "assistant", "message", "text":
		if s, ok := m["text"].(string); ok {
			ev.Text = s
		}
	}
	return ev
}
