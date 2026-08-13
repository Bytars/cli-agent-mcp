// SPDX-License-Identifier: Apache-2.0

// Package inspect reads the task store from outside the server process.
//
// Everything the server knows already lands on disk as it happens: one JSON
// record per task, rewritten at every transition, and one transcript appended
// line by line while the worker is still running. That makes a second,
// read-only process enough to watch any task live — no port to open in the
// server, no handshake, and nothing that can disturb a run in flight. It also
// means this works when the server was launched by a GUI client (Claude Desktop,
// Cowork) whose stdio you can never see.
//
// Both front-ends are built on this one Source: the `logs` terminal command and
// the `ui` local web viewer. Neither can change anything — cancelling a task
// requires the process that owns the worker, so that stays with the MCP tools
// and the in-conversation board.
package inspect

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
	"github.com/Bytars/cli-agent-mcp/internal/config"
	"github.com/Bytars/cli-agent-mcp/internal/state"
	"github.com/Bytars/cli-agent-mcp/internal/task"
)

// maxCachedLogs bounds how many transcripts are held decoded in memory. The web
// viewer serves incremental slices by line index, which needs the lines it has
// already handed out; without a bound, browsing a long history would keep every
// transcript ever opened.
const maxCachedLogs = 8

// Source is a read-only view of one state directory.
// Its methods are safe for concurrent use.
type Source struct {
	store *state.Store
	reg   *agent.Registry

	mu    sync.Mutex
	cache map[string]*logView
	order []string // cache keys, least recently used first
}

// logView keeps a transcript decoded, plus the cursor that extends it. Re-reading
// the whole file on every poll would work and be much simpler, but the web
// viewer polls once a second per open task and a transcript can be megabytes.
type logView struct {
	f     *state.Follower
	lines []string
}

// Open prepares a read-only view of dir. An empty dir resolves the same way the
// server does: CLI_AGENT_MCP_STATE_DIR, then the per-user default.
func Open(dir string) (*Source, error) {
	cfg := config.Load()
	if strings.TrimSpace(dir) == "" {
		dir = cfg.StateDir
	}
	store, err := state.Open(dir)
	if err != nil {
		return nil, err
	}
	// The registry exists only for ParseLine, so a transcript renders here
	// exactly as it does in the tools. Built from the same config as the server
	// so a custom agent keeps the name its tasks were recorded under.
	reg := agent.NewRegistry(
		agent.NewClaudeAdapter(cfg.ClaudeBin, cfg.PermissionMode, cfg.AllowedTools, cfg.DisallowedTools, cfg.AppendSystemPrompt, cfg.ClaudeExtraArgs),
		agent.NewCursorAdapter(cfg.CursorBin, cfg.CursorExtraArgs),
		agent.NewCustomAdapter(cfg.CustomName, cfg.CustomBin, cfg.CustomArgs),
		agent.NewMockAdapter(),
	)
	return &Source{store: store, reg: reg, cache: map[string]*logView{}}, nil
}

// Dir reports the directory being read. Every front-end prints it, because the
// most common reason for an empty listing is that the server was launched with
// a different CLI_AGENT_MCP_STATE_DIR than the shell running this.
func (s *Source) Dir() string { return s.store.Dir() }

// Close releases the store's handles.
func (s *Source) Close() { s.store.Close() }

// Owner reports the live server process that owns this directory, or nil when
// none is running. Purely informational: it tells a viewer whether new output
// should be expected at all.
func (s *Source) Owner() *state.Owner { return s.store.Owner() }

// Tasks returns every stored task, newest first.
func (s *Source) Tasks() ([]task.Snapshot, error) {
	records, err := s.store.LoadTasks()
	if err != nil {
		return nil, err
	}
	out := make([]task.Snapshot, 0, len(records))
	for _, raw := range records {
		var snap task.Snapshot
		if json.Unmarshal(raw, &snap) != nil || snap.ID == "" {
			continue // one unreadable record must not cost the rest of the history
		}
		out = append(out, snap)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := parseTime(out[i].StartedAt), parseTime(out[j].StartedAt)
		if a.Equal(b) {
			return out[i].ID > out[j].ID
		}
		return a.After(b)
	})
	return out, nil
}

// Task returns one task's current record. It re-reads from disk on every call:
// the owning process keeps rewriting the file, so this is how a viewer learns
// that a run finished.
func (s *Source) Task(id string) (task.Snapshot, bool) {
	raw, err := s.store.LoadTask(id)
	if err != nil || raw == nil {
		return task.Snapshot{}, false
	}
	var snap task.Snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.ID == "" {
		return task.Snapshot{}, false
	}
	return snap, true
}

// Resolve turns a user-typed reference into a task. It accepts a full id, any
// unambiguous fragment of one, "latest" for the most recent task, or "running"
// for the one currently in flight — because nobody wants to retype
// task-12-9f3a1c04 to look at the thing they just started.
func (s *Source) Resolve(ref string) (task.Snapshot, error) {
	tasks, err := s.Tasks()
	if err != nil {
		return task.Snapshot{}, err
	}
	if len(tasks) == 0 {
		return task.Snapshot{}, fmt.Errorf("no tasks recorded in %s", s.Dir())
	}

	ref = strings.TrimSpace(ref)
	switch strings.ToLower(ref) {
	case "":
		return task.Snapshot{}, fmt.Errorf("no task given")
	case "latest", "last", "newest":
		return tasks[0], nil
	case "running", "active", "in-flight":
		var live []task.Snapshot
		for _, t := range tasks {
			if t.Status == task.StatusRunning {
				live = append(live, t)
			}
		}
		switch len(live) {
		case 0:
			return task.Snapshot{}, fmt.Errorf("no task is running")
		case 1:
			return live[0], nil
		default:
			return task.Snapshot{}, fmt.Errorf("%d tasks are running; say which one (or use --all)", len(live))
		}
	}

	for _, t := range tasks {
		if t.ID == ref {
			return t, nil
		}
	}

	var hits []task.Snapshot
	for _, t := range tasks {
		if strings.Contains(t.ID, ref) {
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 0:
		return task.Snapshot{}, fmt.Errorf("no task matches %q (run `cli-agent-mcp tasks` to list them)", ref)
	case 1:
		return hits[0], nil
	default:
		ids := make([]string, 0, len(hits))
		for _, t := range hits {
			ids = append(ids, t.ID)
		}
		return task.Snapshot{}, fmt.Errorf("%q matches several tasks: %s", ref, strings.Join(ids, ", "))
	}
}

// Adapter returns the adapter that produced a task's output, or nil when that
// agent is no longer configured here.
func (s *Source) Adapter(name string) agent.Adapter { return s.reg.Get(name) }

// Follow opens an independent cursor over a task's transcript. The terminal
// command uses this directly: it streams forward and never needs to re-read.
func (s *Source) Follow(id string, lastN int) (*state.Follower, error) {
	return s.store.Follow(id, lastN)
}

// Lines returns the task's transcript from line index `since`, plus the total
// number of lines now on disk. Indices address the raw stream and never shift,
// so a caller keeps polling with the returned total as its next cursor — the
// same contract agent_get_output offers.
func (s *Source) Lines(id string, since int) (lines []string, total int, err error) {
	if err := validID(id); err != nil {
		return nil, 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok := s.cache[id]
	if !ok {
		f, err := s.store.Follow(id, -1) // from the beginning: indices must match the server's
		if err != nil {
			return nil, 0, err
		}
		v = &logView{f: f}
		s.cache[id] = v
		s.order = append(s.order, id)
		s.evictLocked()
	} else {
		s.touchLocked(id)
	}

	fresh, err := v.f.Next()
	if err != nil {
		return nil, 0, err
	}
	v.lines = append(v.lines, fresh...)

	total = len(v.lines)
	if since < 0 {
		since = 0
	}
	if since > total {
		since = total
	}
	out := make([]string, total-since)
	copy(out, v.lines[since:])
	return out, total, nil
}

func (s *Source) touchLocked(id string) {
	for i, k := range s.order {
		if k == id {
			s.order = append(append(s.order[:i], s.order[i+1:]...), id)
			return
		}
	}
}

func (s *Source) evictLocked() {
	for len(s.order) > maxCachedLogs {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.cache, oldest)
	}
}

// validID mirrors the store's own rule. Lines is reachable from an HTTP handler,
// so the id arrives from outside the process; it must never be able to name a
// path.
func validID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("no task id given")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return fmt.Errorf("invalid task id")
		}
	}
	return nil
}

// Render turns raw transcript lines into what the user reads. In compact mode
// noise (init dumps, config chatter) renders as "" and is dropped here, exactly
// as the tools drop it; raw mode returns the agent's own JSONL untouched.
func (s *Source) Render(agentName string, raw []string, compact bool) []string {
	if !compact {
		return raw
	}
	a := s.Adapter(agentName)
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if r := strings.TrimSpace(task.RenderLine(a, line)); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func parseTime(s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// Elapsed is how long a task has been running, or how long it ran.
func Elapsed(t task.Snapshot, now time.Time) time.Duration {
	start := parseTime(t.StartedAt)
	if start.IsZero() {
		return 0
	}
	end := parseTime(t.EndedAt)
	if end.IsZero() {
		end = now
	}
	if end.Before(start) {
		return 0
	}
	return end.Sub(start)
}

// FormatDuration renders a duration the way a status line wants it: seconds
// while that is still informative, then minutes, then hours.
func FormatDuration(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
}

// LastPrompt is the instruction the task is currently working on, for one-line
// listings.
func LastPrompt(t task.Snapshot) string {
	if n := len(t.Prompts); n > 0 {
		return strings.TrimSpace(t.Prompts[n-1])
	}
	return ""
}
