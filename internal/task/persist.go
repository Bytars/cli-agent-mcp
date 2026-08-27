// SPDX-License-Identifier: Apache-2.0

package task

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
	"github.com/Bytars/cli-agent-mcp/internal/state"
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
// foreign is the previous owner Store.Acquire reported: another server process
// that is still alive and running tasks of its own. It is passed in rather than
// read here because Acquire has already overwritten the lock with this process's
// pid by the time Restore runs — asking the store afterwards returns us.
func (m *Manager) Restore(reg *agent.Registry, foreign *state.Owner) int {
	if m.store == nil {
		return 0
	}

	// Sessions do not share their agents (issue #21).
	//
	// Adopting every record is right after a restart — that is how a client
	// closed and reopened keeps its history — and wrong while another server is
	// still running. There it pulled a live session's tasks into this one, where
	// they appeared as "orphaned" and could be cancelled from a window that had
	// never started them. Two windows of the same client are two sessions.
	//
	// foreign is the right discriminator precisely because Acquire only reports
	// it when the recorded pid is ALIVE: a crashed instance leaves a stale lock,
	// foreign is nil, and the restart path is untouched.
	//
	// THE PRICE, STATED: records left by an instance that dies while this one
	// runs are not adopted here. They wait for the next start that finds no live
	// owner. Nothing is lost — the files stay, and `cli-agent-mcp logs --all`
	// reads them from outside regardless.
	if foreign != nil {
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

// refreshOrphan re-reads a task that belongs to another server process.
//
// Restoring it once at startup would freeze it at that instant: the owning
// process keeps appending to the transcript and keeps rewriting the record, so
// without this an orphan would sit unchanged forever, even after its worker had
// finished and written a result. Re-reading lets it advance, and lets it settle
// into its real outcome.
//
// Throttled to once a second, because the board polls the task list every two.
func (t *Task) refreshOrphan() {
	t.mu.Lock()
	if t.store == nil || t.status != StatusOrphaned || time.Since(t.lastRefresh) < time.Second {
		t.mu.Unlock()
		return
	}
	t.lastRefresh = time.Now()
	store, adapter, have := t.store, t.adapter, len(t.lines)
	t.mu.Unlock()

	// Read off the lock: this is file I/O on paths another process is writing.
	raw, err := store.LoadTask(t.ID)
	if err != nil || raw == nil {
		return
	}
	var snap Snapshot
	if json.Unmarshal(raw, &snap) != nil {
		return
	}
	lines, _ := store.ReadLines(t.ID)

	t.mu.Lock()
	defer t.mu.Unlock()

	// Append only what is new. Rebuilding the whole slice would shift indices
	// that callers are already holding as since_line.
	for i := have; i < len(lines); i++ {
		stderr := strings.HasPrefix(lines[i], "[stderr] ")
		t.lines = append(t.lines, lines[i])
		t.fromStderr = append(t.fromStderr, stderr)
		t.display = append(t.display, renderRestored(lines[i], stderr, adapter))
	}

	// A settled record means the owning process saw the worker finish, so the
	// outcome is knowable now and the task stops being an orphan.
	if snap.Status != "" && snap.Status != StatusRunning && snap.Status != StatusOrphaned {
		t.status = snap.Status
		t.resultText = snap.Result
		t.isError = snap.IsError
		t.exitCode = snap.ExitCode
		t.runErr = snap.Error
		t.endedAt = parseTime(snap.EndedAt)
	}
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
