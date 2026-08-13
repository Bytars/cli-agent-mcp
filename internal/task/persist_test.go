package task

import (
	"strings"
	"testing"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
	"github.com/Bytars/cli-agent-mcp/internal/state"
)

// The incident this exists for: a second server instance came up with an empty
// registry while the first one's workers kept running, so agent_list_tasks
// reported "nothing here" as fact. A new manager over the same store must see
// the earlier tasks, and must not pretend it can still watch them.
func TestRestoreSurfacesPreviousInstanceTasksAsOrphaned(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// What the previous process left behind: a task still marked running.
	first := NewManager(10)
	first.SetStore(store)
	tk := &Task{
		ID:        "task-1-abcd",
		AgentName: "mock",
		Cwd:       t.TempDir(),
		store:     store,
		status:    StatusRunning,
		running:   true,
		sessionID: "sess-1",
		startedAt: time.Now(),
		turns:     []TurnInfo{{Prompt: "do the thing"}},
	}
	tk.mu.Lock()
	tk.appendLine(`{"type":"assistant","text":"hello"}`, "hello", false)
	tk.appendLine("[stderr] boom", "⚠ boom", true)
	tk.mu.Unlock()
	tk.persist()

	// A fresh process over the same directory.
	second := NewManager(10)
	second.SetStore(store)
	if n := second.Restore(agent.NewRegistry(agent.NewMockAdapter())); n != 1 {
		t.Fatalf("restored %d tasks, want 1", n)
	}

	got, ok := second.Get("task-1-abcd")
	if !ok {
		t.Fatal("the task from the previous instance is still invisible")
	}
	snap := got.Snapshot()

	if snap.Status != StatusOrphaned {
		t.Errorf("status = %q, want %q — this process cannot watch it finish", snap.Status, StatusOrphaned)
	}
	if snap.TotalLines != 2 {
		t.Errorf("total_output_lines = %d, want 2", snap.TotalLines)
	}
	if snap.SessionID != "sess-1" {
		t.Errorf("session_id = %q, want it preserved", snap.SessionID)
	}
	if len(snap.Prompts) != 1 || snap.Prompts[0] != "do the thing" {
		t.Errorf("prompts = %v, want the original prompt", snap.Prompts)
	}
	if len(second.List()) != 1 {
		t.Error("agent_list_tasks would still report an empty registry")
	}

	// The transcript has to come back too, or the record is a stub.
	_, _, _, raw := got.Output(0, 0, false)
	if !strings.Contains(raw, `"text":"hello"`) {
		t.Errorf("restored transcript lost the agent's output:\n%s", raw)
	}
	// Raw mode promises JSONL, so stderr must stay out of it. That it does proves
	// the per-line stderr flag survived the round-trip rather than every restored
	// line coming back as plain stdout.
	if strings.Contains(raw, "[stderr]") {
		t.Errorf("restored lines lost their stderr flag, polluting the JSONL view:\n%s", raw)
	}
	if _, _, _, compact := got.Output(0, 0, true); !strings.Contains(compact, "boom") {
		t.Errorf("compact view lost the stderr line:\n%s", compact)
	}

	// Resuming would put a second worker on a session the old instance may still
	// be driving, so it must be refused with an explanation rather than silently
	// attempted.
	_, err = second.Followup("task-1-abcd", "keep going", nil, nil)
	if err == nil {
		t.Fatal("a follow-up on an orphaned task was accepted")
	}
	if !strings.Contains(err.Error(), "previous server instance") {
		t.Errorf("the refusal does not explain why: %v", err)
	}
}

// Restoring an orphan once would freeze it at that instant. The process that
// owns it keeps writing, so this one has to keep re-reading — otherwise a task
// that finished elsewhere sits at "orphaned" forever and the user never learns
// how it went.
func TestOrphanPicksUpProgressFromTheOwningProcess(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	// The other process: a task mid-run.
	owner := NewManager(10)
	owner.SetStore(store)
	theirs := &Task{
		ID: "task-9-cafe", AgentName: "mock", store: store,
		status: StatusRunning, running: true, startedAt: time.Now(),
	}
	theirs.mu.Lock()
	theirs.appendLine("line one", "line one", false)
	theirs.mu.Unlock()
	theirs.persist()

	// This process picks it up as an orphan.
	mine := NewManager(10)
	mine.SetStore(store)
	mine.Restore(agent.NewRegistry(agent.NewMockAdapter()))
	got, ok := mine.Get("task-9-cafe")
	if !ok {
		t.Fatal("the other process's task was not restored")
	}
	if s := got.Snapshot(); s.Status != StatusOrphaned {
		t.Fatalf("status = %q, want %q", s.Status, StatusOrphaned)
	}

	// The other process keeps working, then finishes.
	theirs.mu.Lock()
	theirs.appendLine("line two", "line two", false)
	theirs.status = StatusDone
	theirs.running = false
	theirs.resultText = "finished elsewhere"
	theirs.endedAt = time.Now()
	theirs.mu.Unlock()
	theirs.persist()

	// Re-reads are throttled to once a second, so wait past that window.
	time.Sleep(1100 * time.Millisecond)

	snap := got.Snapshot()
	if snap.Status != StatusDone {
		t.Errorf("status = %q, want %q once the owner settled it", snap.Status, StatusDone)
	}
	if snap.Result != "finished elsewhere" {
		t.Errorf("result = %q, want the outcome the other process recorded", snap.Result)
	}
	if snap.TotalLines != 2 {
		t.Errorf("total_output_lines = %d, want 2 — the later line never arrived", snap.TotalLines)
	}
}

// A finished task must come back finished, not relabelled as orphaned.
func TestRestoreKeepsSettledOutcomes(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	m := NewManager(10)
	m.SetStore(store)
	exit := 0
	done := &Task{
		ID: "task-2-beef", AgentName: "mock", store: store,
		status: StatusDone, resultText: "all tests pass", exitCode: &exit,
		startedAt: time.Now().Add(-time.Minute), endedAt: time.Now(),
	}
	done.persist()

	next := NewManager(10)
	next.SetStore(store)
	next.Restore(agent.NewRegistry(agent.NewMockAdapter()))

	got, ok := next.Get("task-2-beef")
	if !ok {
		t.Fatal("finished task was not restored")
	}
	snap := got.Snapshot()
	if snap.Status != StatusDone {
		t.Errorf("status = %q, want %q", snap.Status, StatusDone)
	}
	if snap.Result != "all tests pass" {
		t.Errorf("result = %q, want it preserved", snap.Result)
	}
	if snap.EndedAt == "" {
		t.Error("ended_at was lost, so the task looks like it never finished")
	}
}
