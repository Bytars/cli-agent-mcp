package task

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/andresh0816/cli-agent-mcp/internal/agent"
	"github.com/andresh0816/cli-agent-mcp/internal/state"
)

// SetStore attaches durable storage. Without one the manager behaves exactly as
// it did before, keeping everything in memory; every persistence call below is
// a no-op on a nil store.
func (m *Manager) SetStore(s *state.Store) { m.store = s }

// persist writes a task's current state to disk. Failures are deliberately
// silent: losing the ability to recover a record later must never take down the
// run that is producing it.
func (t *Task) persist() {
	if t == nil || t.store == nil {
		return
	}
	_ = t.store.SaveTask(t.ID, t.Snapshot())
}

// Restore rebuilds the tasks left behind by a previous server process.
//
// A record still marked running belonged to a process that is not this one, so
// its real outcome is unknowable from here: the worker may have finished, or
// may still be going under the old instance. Calling it "running" would invite
// agent_watch to block forever on something this process can never see finish,
// so it becomes StatusOrphaned instead — readable, honest, and clearly not
// something to wait on.
//
// Restored tasks reattach to their adapter by name, so one that captured a
// session id can still be resumed with a follow-up.
func (m *Manager) Restore(reg *agent.Registry) int {
	if m.store == nil {
		return 0
	}
	records, err := m.store.LoadTasks()
	if err != nil {
		return 0
	}

	restored := 0
	for _, raw := range records {
		var snap Snapshot
		if json.Unmarshal(raw, &snap) != nil || snap.ID == "" {
			continue
		}

		m.mu.Lock()
		_, exists := m.tasks[snap.ID]
		m.mu.Unlock()
		if exists {
			continue
		}

		t := m.taskFromSnapshot(snap, reg)
		m.mu.Lock()
		m.tasks[t.ID] = t
		m.order = append(m.order, t.ID)
		m.mu.Unlock()
		restored++
	}

	m.mu.Lock()
	m.evictLocked()
	m.mu.Unlock()
	return restored
}

func (m *Manager) taskFromSnapshot(snap Snapshot, reg *agent.Registry) *Task {
	status := snap.Status
	if status == StatusRunning {
		status = StatusOrphaned
	}

	t := &Task{
		ID:         snap.ID,
		AgentName:  snap.Agent,
		Cwd:        snap.Cwd,
		Model:      snap.Model,
		audit:      m.audit,
		store:      m.store,
		status:     status,
		sessionID:  snap.SessionID,
		resultText: snap.Result,
		isError:    snap.IsError,
		exitCode:   snap.ExitCode,
		runErr:     snap.Error,
		startedAt:  parseTime(snap.StartedAt),
		endedAt:    parseTime(snap.EndedAt),
	}
	if reg != nil {
		t.adapter = reg.Get(snap.Agent)
	}
	for _, p := range snap.Prompts {
		t.turns = append(t.turns, TurnInfo{Prompt: p, StartedAt: t.startedAt})
	}

	lines, err := m.store.ReadLines(snap.ID)
	if err != nil {
		return t
	}
	for _, line := range lines {
		stderr := strings.HasPrefix(line, "[stderr] ")
		t.lines = append(t.lines, line)
		t.fromStderr = append(t.fromStderr, stderr)
		t.display = append(t.display, renderRestored(line, stderr, t.adapter))
	}
	return t
}

// renderRestored reproduces the compact rendering a line had while it was
// streaming, so a restored transcript reads the same as a live one.
func renderRestored(line string, stderr bool, a agent.Adapter) string {
	if stderr {
		return "⚠ " + strings.TrimSpace(strings.TrimPrefix(line, "[stderr] "))
	}
	if a == nil {
		return line
	}
	return renderEvent(a.ParseLine(line))
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return ts
}
