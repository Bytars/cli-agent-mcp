// Package task runs CLI-agent turns as background jobs and tracks their state.
//
// A Task owns one agent *session*: the first turn starts it, and follow-up
// turns resume it (so a delegating model can hold a multi-turn conversation with
// the worker agent). Each turn spawns the agent headless; the process inherits
// the server's environment, so the VPN and the 1Password SSH agent are visible
// to the child with zero extra wiring.
package task

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andresh0816/cli-agent-mcp/internal/agent"
)

// Status is the lifecycle state of a task's most recent turn.
type Status string

const (
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

// TurnInfo records one prompt sent within a task.
type TurnInfo struct {
	Prompt    string
	StartLine int
	StartedAt time.Time
}

// Task is a single delegated job. All fields are guarded by mu.
type Task struct {
	ID        string
	AgentName string
	Cwd       string
	Model     string

	mu                sync.Mutex
	status            Status
	sessionID         string
	lines             []string
	resultText        string
	isError           bool
	exitCode          *int
	runErr            string
	startedAt         time.Time
	endedAt           time.Time
	turns             []TurnInfo
	running           bool
	cancel            context.CancelFunc
	canceledRequested bool

	adapter agent.Adapter
}

// Snapshot is an immutable view of a Task for serialization.
type Snapshot struct {
	ID         string   `json:"task_id"`
	Agent      string   `json:"agent"`
	Cwd        string   `json:"cwd"`
	Model      string   `json:"model,omitempty"`
	Status     Status   `json:"status"`
	SessionID  string   `json:"session_id,omitempty"`
	Result     string   `json:"result,omitempty"`
	IsError    bool     `json:"is_error"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Error      string   `json:"error,omitempty"`
	StartedAt  string   `json:"started_at"`
	EndedAt    string   `json:"ended_at,omitempty"`
	TotalLines int      `json:"total_output_lines"`
	Turns      int      `json:"turns"`
	Prompts    []string `json:"prompts,omitempty"`
}

func (t *Task) snapshot() Snapshot {
	s := Snapshot{
		ID:         t.ID,
		Agent:      t.AgentName,
		Cwd:        t.Cwd,
		Model:      t.Model,
		Status:     t.status,
		SessionID:  t.sessionID,
		Result:     t.resultText,
		IsError:    t.isError,
		ExitCode:   t.exitCode,
		Error:      t.runErr,
		StartedAt:  t.startedAt.Format(time.RFC3339),
		TotalLines: len(t.lines),
		Turns:      len(t.turns),
	}
	if !t.endedAt.IsZero() {
		s.EndedAt = t.endedAt.Format(time.RFC3339)
	}
	for _, tr := range t.turns {
		s.Prompts = append(s.Prompts, tr.Prompt)
	}
	return s
}

// Snapshot returns a thread-safe view of the task.
func (t *Task) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot()
}

// Output returns lines[since:since+max]. since is 0-based; max<=0 means "all".
func (t *Task) Output(since, max int) (from, to, total int, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	total = len(t.lines)
	if since < 0 {
		since = 0
	}
	if since > total {
		since = total
	}
	end := total
	if max > 0 && since+max < end {
		end = since + max
	}
	return since, end, total, strings.Join(t.lines[since:end], "\n")
}

// EventSink receives each parsed stdout event of a running turn, in order. It
// is used by the streaming ("run") tools to forward live progress to the MCP
// client. It is always called off the task lock, so it may call back into the
// task safely.
type EventSink func(ev agent.Event)

// Manager owns all tasks.
type Manager struct {
	mu       sync.Mutex
	tasks    map[string]*Task
	order    []string
	maxTasks int
	counter  atomic.Uint64
}

// NewManager builds a task manager retaining up to maxTasks tasks.
func NewManager(maxTasks int) *Manager {
	if maxTasks <= 0 {
		maxTasks = 100
	}
	return &Manager{tasks: make(map[string]*Task), maxTasks: maxTasks}
}

func newID(n uint64) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("task-%d-%s", n, hex.EncodeToString(b[:]))
}

// ResolveCwd validates and normalizes a requested working directory against the
// configured default and optional allow-list.
func ResolveCwd(requested, defaultCwd string, allowed []string) (string, error) {
	cwd := strings.TrimSpace(requested)
	if cwd == "" {
		cwd = defaultCwd
	}
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("invalid cwd %q: %w", cwd, err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("cwd %q is not an existing directory", abs)
	}
	if len(allowed) > 0 {
		ok := false
		for _, root := range allowed {
			if pathWithin(abs, root) {
				ok = true
				break
			}
		}
		if !ok {
			return "", fmt.Errorf("cwd %q is outside the allowed roots", abs)
		}
	}
	return abs, nil
}

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// StartTask creates a task and launches its first turn asynchronously.
func (m *Manager) StartTask(a agent.Adapter, cwd string, spec agent.RunSpec) (*Task, error) {
	t := &Task{
		ID:        newID(m.counter.Add(1)),
		AgentName: a.Name(),
		Cwd:       cwd,
		Model:     spec.Model,
		adapter:   a,
		status:    StatusRunning,
		startedAt: time.Now(),
	}
	spec.Cwd = cwd

	m.mu.Lock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	m.evictLocked()
	m.mu.Unlock()

	go m.runTurn(context.Background(), t, spec, nil)
	return t, nil
}

// RunTaskStreaming creates a task and runs its first turn synchronously,
// forwarding each event to sink as it arrives. It blocks until the turn
// finishes (or ctx is canceled, which terminates the agent) and returns the
// task. Used by the streaming "run" tools so the MCP client sees live progress.
func (m *Manager) RunTaskStreaming(ctx context.Context, a agent.Adapter, cwd string, spec agent.RunSpec, sink EventSink) (*Task, error) {
	t := &Task{
		ID:        newID(m.counter.Add(1)),
		AgentName: a.Name(),
		Cwd:       cwd,
		Model:     spec.Model,
		adapter:   a,
		status:    StatusRunning,
		startedAt: time.Now(),
	}
	spec.Cwd = cwd

	m.mu.Lock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	m.evictLocked()
	m.mu.Unlock()

	m.runTurn(ctx, t, spec, sink)
	return t, nil
}

// FollowupStreaming resumes a finished task's session with a new prompt, running
// synchronously and forwarding events to sink.
func (m *Manager) FollowupStreaming(ctx context.Context, id, prompt string, extraArgs []string, sink EventSink) (*Task, error) {
	t, spec, err := m.prepareFollowup(id, prompt, extraArgs)
	if err != nil {
		return nil, err
	}
	m.runTurn(ctx, t, spec, sink)
	return t, nil
}

// evictLocked drops the oldest finished tasks beyond maxTasks. Caller holds mu.
func (m *Manager) evictLocked() {
	for len(m.order) > m.maxTasks {
		// find the oldest non-running task to drop
		dropped := false
		for i, id := range m.order {
			tk := m.tasks[id]
			tk.mu.Lock()
			running := tk.running
			tk.mu.Unlock()
			if !running {
				delete(m.tasks, id)
				m.order = append(m.order[:i], m.order[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			break // everything is running; keep them all
		}
	}
}

// prepareFollowup validates that a task can be resumed and builds the RunSpec
// for the next turn. It fails if the task is still running or never captured a
// session id.
func (m *Manager) prepareFollowup(id, prompt string, extraArgs []string) (*Task, agent.RunSpec, error) {
	t, ok := m.Get(id)
	if !ok {
		return nil, agent.RunSpec{}, fmt.Errorf("unknown task %q", id)
	}
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return nil, agent.RunSpec{}, errors.New("task is still running; wait for it to finish before sending a follow-up")
	}
	session := t.sessionID
	model := t.Model
	cwd := t.Cwd
	t.mu.Unlock()

	if session == "" {
		return nil, agent.RunSpec{}, errors.New("no session id captured for this task; cannot resume (start a new task instead)")
	}
	return t, agent.RunSpec{
		Prompt:    prompt,
		Cwd:       cwd,
		Model:     model,
		SessionID: session,
		ExtraArgs: extraArgs,
	}, nil
}

// Followup resumes a task's session with a new prompt, asynchronously.
func (m *Manager) Followup(id, prompt string, extraArgs []string) (*Task, error) {
	t, spec, err := m.prepareFollowup(id, prompt, extraArgs)
	if err != nil {
		return nil, err
	}
	go m.runTurn(context.Background(), t, spec, nil)
	return t, nil
}

func (m *Manager) runTurn(parent context.Context, t *Task, spec agent.RunSpec, sink EventSink) {
	ctx, cancel := context.WithCancel(parent)

	t.mu.Lock()
	t.cancel = cancel
	t.status = StatusRunning
	t.running = true
	t.canceledRequested = false
	t.runErr = ""
	t.resultText = ""
	t.isError = false
	t.exitCode = nil
	t.endedAt = time.Time{}
	t.turns = append(t.turns, TurnInfo{Prompt: spec.Prompt, StartLine: len(t.lines), StartedAt: time.Now()})
	t.mu.Unlock()

	fail := func(msg string) {
		t.mu.Lock()
		t.running = false
		t.cancel = nil
		t.status = StatusFailed
		t.runErr = msg
		t.endedAt = time.Now()
		t.appendLine("[error] " + msg)
		t.mu.Unlock()
		cancel()
	}

	cmd, err := t.adapter.Command(ctx, spec)
	if err != nil {
		fail("building command: " + err.Error())
		return
	}
	cmd.Dir = spec.Cwd
	// cmd.Env left nil → child inherits our full environment (VPN, SSH agent).

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fail("stdout pipe: " + err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fail("stderr pipe: " + err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		fail("starting agent: " + err.Error())
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go t.pump(stdout, false, sink, &wg)
	go t.pump(stderr, true, nil, &wg)
	wg.Wait()
	waitErr := cmd.Wait()

	t.mu.Lock()
	t.running = false
	t.cancel = nil
	t.endedAt = time.Now()
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		t.exitCode = &code
	}
	switch {
	case t.canceledRequested:
		t.status = StatusCanceled
	case waitErr == nil && !t.isError:
		t.status = StatusDone
	default:
		t.status = StatusFailed
		if t.runErr == "" && waitErr != nil {
			t.runErr = waitErr.Error()
		}
	}
	t.mu.Unlock()
}

// appendLine adds a line. Caller holds t.mu.
func (t *Task) appendLine(line string) {
	t.lines = append(t.lines, line)
}

// pump reads a stream line by line, parsing agent stdout for structured events.
// For stdout, each parsed event is forwarded to sink (if non-nil) after the task
// lock is released, so a streaming caller sees live progress.
func (t *Task) pump(r io.Reader, isErr bool, sink EventSink, wg *sync.WaitGroup) {
	defer wg.Done()
	br := bufio.NewReaderSize(r, 1024*1024)
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			if isErr {
				t.mu.Lock()
				t.appendLine("[stderr] " + line)
				t.mu.Unlock()
			} else {
				ev := t.adapter.ParseLine(line)
				t.mu.Lock()
				t.appendLine(line)
				if ev.SessionID != "" {
					t.sessionID = ev.SessionID
				}
				if ev.Final {
					t.isError = ev.FinalError
					if ev.FinalText != "" {
						t.resultText = ev.FinalText
					}
				}
				t.mu.Unlock()
				if sink != nil {
					sink(ev)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// Get returns a task by id.
func (m *Manager) Get(id string) (*Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	return t, ok
}

// List returns snapshots of all tasks, newest first.
func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	ids := make([]string, len(m.order))
	copy(ids, m.order)
	tasks := m.tasks
	m.mu.Unlock()

	out := make([]Snapshot, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		if t, ok := tasks[ids[i]]; ok {
			out = append(out, t.Snapshot())
		}
	}
	return out
}

// Cancel requests cancellation of a running task.
func (m *Manager) Cancel(id string) (Snapshot, error) {
	t, ok := m.Get(id)
	if !ok {
		return Snapshot{}, fmt.Errorf("unknown task %q", id)
	}
	t.mu.Lock()
	if !t.running {
		s := t.snapshot()
		t.mu.Unlock()
		return s, nil
	}
	t.canceledRequested = true
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Give the run goroutine a moment to transition.
	time.Sleep(50 * time.Millisecond)
	return t.Snapshot(), nil
}
