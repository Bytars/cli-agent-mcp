// SPDX-License-Identifier: Apache-2.0

// Package task runs CLI-agent turns as background jobs and tracks their state.
//
// A Task owns one agent *session*: the first turn starts it, and follow-up
// turns resume it (so a delegating model can hold a multi-turn conversation with
// the worker agent). Each turn spawns the agent headless; the process inherits
// the server's environment, so whatever the host machine can reach (VPN routes,
// an SSH agent, credentials) is available to the worker with zero extra wiring.
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
	"unicode/utf8"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
	"github.com/Bytars/cli-agent-mcp/internal/audit"
	"github.com/Bytars/cli-agent-mcp/internal/grants"
	"github.com/Bytars/cli-agent-mcp/internal/state"
)

// Status is the lifecycle state of a task's most recent turn.
type Status string

const (
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"

	// StatusOrphaned marks a task restored from disk that a previous server
	// process was still running. This process cannot watch it, cancel it, or
	// learn how it ended — the worker may have finished long ago or may still be
	// going under the old instance. Reporting it as "running" would be a lie
	// that makes agent_watch block forever.
	StatusOrphaned Status = "orphaned"
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
	lines             []string // raw output lines
	display           []string // aligned with lines: compact rendering, "" = noise
	fromStderr        []bool   // aligned with lines: this line came from stderr
	truncated         bool     // transcript hit its cap; no more lines are kept
	resultText        string
	lastText          string // last human-facing text event of the current turn
	isError           bool
	exitCode          *int
	runErr            string
	startedAt         time.Time
	endedAt           time.Time
	turns             []TurnInfo
	running           bool
	cancel            context.CancelFunc
	canceledRequested bool
	timedOut          bool

	adapter agent.Adapter
	audit   *audit.Logger
	store   *state.Store

	// lastRefresh throttles re-reading an orphan's files; see refreshOrphan.
	lastRefresh time.Time

	// pending is the permission request this task is currently blocked on, if
	// any. It is what turns "the task is running but nothing is happening" into
	// something a person can act on.
	pending *PermissionRequest

	// approver, when set, lets each turn hand the worker a way to ask a human
	// before it gives up on a tool it is not allowed to use.
	approver Approver

	usage     agent.Usage // accumulated over every turn of this task
	modelUsed string      // the model the agent said it was running
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

	// Pending is set while the worker is blocked waiting for someone to allow a
	// tool call. A task in that state is running and getting nowhere, which is
	// indistinguishable from a slow one unless it is said outright.
	Pending *PermissionRequest `json:"pending_permission,omitempty"`

	// ModelUsed is what the agent reported it actually ran, which is the only
	// way to know when the caller requested no particular model.
	ModelUsed string `json:"model_used,omitempty"`

	// Usage accumulates every turn's accounting. Nil when the agent reported
	// none, so a caller can tell "free" from "not measured".
	Usage *agent.Usage `json:"usage,omitempty"`
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
	s.Pending = t.pending
	s.ModelUsed = t.modelUsed
	if !t.usage.Empty() {
		u := t.usage
		s.Usage = &u
	}
	for _, tr := range t.turns {
		s.Prompts = append(s.Prompts, tr.Prompt)
	}
	return s
}

// Snapshot returns a thread-safe view of the task.
func (t *Task) Snapshot() Snapshot {
	t.refreshOrphan()
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot()
}

// WatchFrom blocks until new output appears past `since`, the task finishes, or
// timeout elapses (or ctx is cancelled) — then returns the new lines and state.
// It is the primitive for supervised "director" mode: an orchestrator watches a
// backgrounded task in near-real-time and decides whether to let it continue or
// cancel it, without busy-polling.
func (t *Task) WatchFrom(ctx context.Context, since int, timeout time.Duration, compact bool) (text string, newSince, total int, status Status, running bool) {
	t.refreshOrphan()
	deadline := time.Now().Add(timeout)
	for {
		t.mu.Lock()
		total = len(t.lines)
		status = t.status
		running = t.running
		if since < 0 {
			since = 0
		}
		if since > total {
			since = total
		}
		ready := total > since || !running || time.Now().After(deadline)
		if ready {
			out := t.joinRange(since, total, compact)
			t.mu.Unlock()
			return out, total, total, status, running
		}
		t.mu.Unlock()

		select {
		case <-ctx.Done():
			return "", since, total, status, running
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// Output returns lines[since:since+max]. since is 0-based; max<=0 means "all".
// When compact, noisy lines are dropped and each is rendered human-readably;
// `since`/`total` always index the raw stream so the contract is stable.
func (t *Task) Output(since, max int, compact bool) (from, to, total int, text string) {
	t.refreshOrphan()
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
	return since, end, total, t.joinRange(since, end, compact)
}

// joinRange renders lines[from:to] as compact display or raw JSONL. Caller holds mu.
func (t *Task) joinRange(from, to int, compact bool) string {
	if !compact {
		// Raw mode promises the agent's JSONL transcript. stderr lines are not
		// JSON, so emitting them here breaks any client that parses the result.
		// They remain visible in compact mode and in the audit log.
		var raw []string
		for i := from; i < to && i < len(t.lines); i++ {
			if i < len(t.fromStderr) && t.fromStderr[i] {
				continue
			}
			raw = append(raw, t.lines[i])
		}
		return strings.Join(raw, "\n")
	}
	var kept []string
	for i := from; i < to && i < len(t.display); i++ {
		if s := strings.TrimSpace(t.display[i]); s != "" {
			kept = append(kept, t.display[i])
		}
	}
	return strings.Join(kept, "\n")
}

// EventSink receives each parsed stdout event of a running turn, in order. It
// is used by the streaming ("run") tools to forward live progress to the MCP
// client. It is always called off the task lock, so it may call back into the
// task safely.
type EventSink func(ev agent.Event)

// Options are the per-call knobs shared by every way of starting a turn.
type Options struct {
	// Sink receives each event as it arrives, for callers streaming progress.
	Sink EventSink

	// Window bounds how long a blocking call waits before handing back a task
	// id. Zero means wait until the turn ends or the caller goes away.
	Window time.Duration

	// Approver, when set, lets the worker ask for permission during the run.
	Approver Approver
}

// Approver supplies the wiring that lets a worker ask a human for permission
// mid-run, rather than stalling on a prompt nobody is there to answer.
type Approver interface {
	// Grant issues per-run approval for a task. ok is false when the run should
	// proceed without it — because the orchestrating client cannot ask, or
	// because the operator turned it off. release must be called when the turn
	// ends, and revokes the grant.
	Grant(taskID string) (configPath, toolName string, release func(), ok bool)
}

// Manager owns all tasks.
type Manager struct {
	mu       sync.Mutex
	tasks    map[string]*Task
	order    []string
	maxTasks int
	counter  atomic.Uint64
	audit    *audit.Logger
	store    *state.Store
	timeout  time.Duration

	// maxConcurrent caps live workers. Zero means no limit.
	maxConcurrent int

	// maxCostUSD bounds what one task may spend across all its turns. Zero
	// means no limit.
	maxCostUSD float64

	// desk holds the permission requests workers are currently blocked on.
	desk *permissionDesk

	// grants are the permissions the user granted permanently.
	grants *grants.Store
}

// NewManager builds a task manager retaining up to maxTasks tasks.
func NewManager(maxTasks int) *Manager {
	if maxTasks <= 0 {
		maxTasks = 100
	}
	return &Manager{tasks: make(map[string]*Task), maxTasks: maxTasks, desk: newDesk()}
}

// SetAudit attaches an audit logger; nil or a disabled logger is fine.
func (m *Manager) SetAudit(a *audit.Logger) { m.audit = a }

// SetTaskTimeout sets a per-turn timeout; zero disables it.
func (m *Manager) SetTaskTimeout(d time.Duration) { m.timeout = d }

// SetMaxConcurrent caps how many workers may run at once; zero disables it.
func (m *Manager) SetMaxConcurrent(n int) { m.maxConcurrent = n }

// SetGrants attaches the store of permissions the user has granted permanently.
func (m *Manager) SetGrants(g *grants.Store) { m.grants = g }

// SetMaxCostUSD bounds what a single task may spend; zero disables it.
func (m *Manager) SetMaxCostUSD(v float64) { m.maxCostUSD = v }

// budgetFor returns what this turn may spend and whether it may run at all.
//
// The figure is what the task has LEFT, not the configured total, because the
// agent applies it per invocation: handing a task its full allowance on every
// follow-up would mean ten turns cost ten budgets. Once nothing is left the
// turn is refused outright rather than started with an allowance of zero, which
// the agent would read as "no limit".
func (m *Manager) budgetFor(t *Task) (remaining float64, ok bool) {
	if m.maxCostUSD <= 0 {
		return 0, true
	}
	t.mu.Lock()
	spent := t.usage.CostUSD
	t.mu.Unlock()

	if left := m.maxCostUSD - spent; left > 0 {
		return left, true
	}
	return 0, false
}

// Running reports how many workers are alive right now.
func (m *Manager) Running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, id := range m.order {
		tk := m.tasks[id]
		tk.mu.Lock()
		if tk.running {
			n++
		}
		tk.mu.Unlock()
	}
	return n
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
func (m *Manager) StartTask(a agent.Adapter, cwd string, spec agent.RunSpec, opts Options) (*Task, error) {
	t := m.newTask(a, cwd, spec)
	t.approver = opts.Approver
	spec.Cwd = cwd

	if err := m.admit(t); err != nil {
		return nil, err
	}

	t.persist()

	go m.runTurn(context.Background(), t, spec, nil)
	return t, nil
}

// RunTaskStreaming creates a task and runs its first turn, forwarding each
// event to opts.Sink as it arrives. It waits for the turn under the rules in
// runDetached and reports whether it finished; either way the turn keeps
// running. Used by the streaming "run" tools so the MCP client sees live
// progress.
func (m *Manager) RunTaskStreaming(ctx context.Context, a agent.Adapter, cwd string, spec agent.RunSpec, opts Options) (t *Task, finished bool, err error) {
	t = m.newTask(a, cwd, spec)
	t.approver = opts.Approver
	spec.Cwd = cwd

	if err := m.admit(t); err != nil {
		return nil, false, err
	}

	t.persist()

	return t, m.runDetached(ctx, t, spec, opts), nil
}

// FollowupStreaming resumes a finished task's session with a new prompt,
// waiting on it under the same rules as RunTaskStreaming.
func (m *Manager) FollowupStreaming(ctx context.Context, id, prompt string, allowedTools, extraArgs []string, opts Options) (*Task, bool, error) {
	t, spec, err := m.prepareFollowup(id, prompt, allowedTools, extraArgs)
	if err != nil {
		return nil, false, err
	}
	if opts.Approver != nil {
		t.mu.Lock()
		t.approver = opts.Approver
		t.mu.Unlock()
	}
	return t, m.runDetached(ctx, t, spec, opts), nil
}

// newTask builds a task record. Every entry point starts from here, so the
// fields a task is born with are defined once rather than per caller.
func (m *Manager) newTask(a agent.Adapter, cwd string, spec agent.RunSpec) *Task {
	return &Task{
		ID:        newID(m.counter.Add(1)),
		AgentName: a.Name(),
		Cwd:       cwd,
		Model:     spec.Model,
		adapter:   a,
		audit:     m.audit,
		store:     m.store,
		status:    StatusRunning,
		running:   true,
		startedAt: time.Now(),
	}
}

// runDetached starts a turn on a context of its own and waits for it, reporting
// whether it finished within the window. The turn keeps running either way.
//
// The turn deliberately does not inherit ctx. ctx belongs to the MCP call that
// asked for the work, and that call has a much shorter life than the work does:
// a client that gives up waiting, or a user who closes the panel, cancels it.
// Running the turn under it meant the worker was killed mid-edit whenever the
// caller stopped listening — the task was lost for the sole reason that nobody
// was watching it. ctx still bounds how long *this call* waits, which is all it
// was ever able to speak for.
func (m *Manager) runDetached(ctx context.Context, t *Task, spec agent.RunSpec, opts Options) (finished bool) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runTurn(context.Background(), t, spec, opts.Sink)
	}()

	window := opts.Window
	if window <= 0 {
		select {
		case <-done:
			return true
		case <-ctx.Done():
			return false
		}
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// admit registers a task, refusing it when the machine is already running as
// many workers as the operator allows.
//
// The cap is on live workers, not on retained records: a headless coding agent
// is a heavyweight process, so an orchestrator fanning out ten tasks can take
// the machine down while maxTasks and every per-task limit still look
// perfectly satisfied.
func (m *Manager) admit(t *Task) error {
	if err := m.checkCapacity(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	m.evictLocked()
	return nil
}

// checkCapacity refuses a new worker once the configured limit is reached.
//
// It must be called without any task lock held, since Running takes each task's
// lock to read its state. Two callers can therefore race past a cap of N and
// briefly reach N+1; that is deliberate, because making it exact would mean
// holding the manager lock across task locks in both directions, which is how
// this deadlocks.
func (m *Manager) checkCapacity() error {
	if m.maxConcurrent <= 0 {
		return nil
	}
	if live := m.Running(); live >= m.maxConcurrent {
		return fmt.Errorf("%d task(s) are already running, which is this server's limit "+
			"(CLI_AGENT_MCP_MAX_CONCURRENT=%d). Wait for one to finish, or cancel one with agent_cancel_task",
			live, m.maxConcurrent)
	}
	return nil
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
				if m.store != nil {
					// Dropping it from memory but leaving it on disk would let the
					// next startup restore what was just evicted.
					m.store.Forget(id)
				}
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
func (m *Manager) prepareFollowup(id, prompt string, allowedTools, extraArgs []string) (*Task, agent.RunSpec, error) {
	t, ok := m.Get(id)
	if !ok {
		return nil, agent.RunSpec{}, fmt.Errorf("unknown task %q", id)
	}
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return nil, agent.RunSpec{}, errors.New("task is still running; wait for it to finish before sending a follow-up")
	}
	if t.status == StatusOrphaned {
		t.mu.Unlock()
		return nil, agent.RunSpec{}, errors.New("this task was started by a previous server instance and may still be running under it; " +
			"resuming would put two workers on the same session. Read its output, then start a new task instead")
	}
	if t.adapter == nil {
		t.mu.Unlock()
		return nil, agent.RunSpec{}, fmt.Errorf("agent %q is no longer configured on this server, so this task cannot be resumed", t.AgentName)
	}
	session := t.sessionID
	model := t.Model
	cwd := t.Cwd
	if session == "" {
		t.mu.Unlock()
		return nil, agent.RunSpec{}, errors.New("no session id captured for this task; cannot resume (start a new task instead)")
	}
	// Claim the task before releasing the lock. Checking `running` here and
	// letting runTurn set it later leaves a window in which two concurrent
	// follow-ups both pass this guard and spawn a process against the same
	// session: they interleave into one transcript, and the first to finish
	// clears t.cancel, leaving the second unkillable.
	t.running = true
	t.status = StatusRunning
	t.mu.Unlock()
	return t, agent.RunSpec{
		Prompt:       prompt,
		Cwd:          cwd,
		Model:        model,
		SessionID:    session,
		AllowedTools: allowedTools,
		ExtraArgs:    extraArgs,
	}, nil
}

// Followup resumes a task's session with a new prompt, asynchronously.
func (m *Manager) Followup(id, prompt string, allowedTools, extraArgs []string, opts Options) (*Task, error) {
	t, spec, err := m.prepareFollowup(id, prompt, allowedTools, extraArgs)
	if err != nil {
		return nil, err
	}
	if opts.Approver != nil {
		t.mu.Lock()
		t.approver = opts.Approver
		t.mu.Unlock()
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
	t.timedOut = false
	t.runErr = ""
	t.resultText = ""
	t.lastText = ""
	t.isError = false
	t.exitCode = nil
	t.endedAt = time.Time{}
	turnStart := len(t.lines)
	turnStarted := time.Now()
	t.turns = append(t.turns, TurnInfo{Prompt: spec.Prompt, StartLine: turnStart, StartedAt: turnStarted})
	t.mu.Unlock()

	// Every early-return path must still close the audit trail. Without this a
	// turn that fails to spawn leaves a turn_start with no turn_end, which is
	// exactly the case an operator most needs to see in the log.
	fail := func(msg string) {
		t.mu.Lock()
		t.running = false
		t.cancel = nil
		t.status = StatusFailed
		t.runErr = msg
		t.endedAt = time.Now()
		t.appendLine("[error] "+msg, "✗ "+msg, false)
		t.mu.Unlock()
		cancel()
		t.audit.Log("turn_end", map[string]any{
			"task_id":     t.ID,
			"status":      string(StatusFailed),
			"is_error":    true,
			"error":       msg,
			"duration_ms": time.Since(turnStarted).Milliseconds(),
		})
	}

	// Issue this turn's permission grant, if the orchestrator can answer for it.
	// It is revoked the moment the turn ends: the URL it names can approve
	// commands on this machine, so it must outlive nothing.
	t.mu.Lock()
	approver := t.approver
	t.mu.Unlock()
	if approver != nil {
		if cfgPath, tool, release, ok := approver.Grant(t.ID); ok {
			defer release()
			spec.MCPConfigPath = cfgPath
			spec.PermissionTool = tool
		}
	}

	// Refusing here rather than at the tool boundary is deliberate: a follow-up
	// is the path that spends a budget repeatedly, and it reaches runTurn from
	// several entry points that would each have to remember the check.
	if left, ok := m.budgetFor(t); !ok {
		t.mu.Lock()
		spent := t.usage.CostUSD
		t.mu.Unlock()
		fail(fmt.Sprintf("this task has spent $%.4f, which is at or past its budget of $%.2f "+
			"(CLI_AGENT_MCP_MAX_COST_USD). Nothing was run. Start a new task, or raise the limit.",
			spent, m.maxCostUSD))
		return
	} else if left > 0 {
		spec.MaxCostUSD = left
	}

	// A permission the user granted permanently is handed to the agent as a
	// pre-approved tool, so the worker never reaches the point of asking for it
	// again. Answering the same question twice is the fastest way to train
	// someone into approving everything without reading it.
	//
	// The desk applies the same grants a second time, for the requests that do
	// reach it. That redundancy is deliberate: this path only saves a round trip
	// to the approval endpoint, and a grant must still be honoured when a
	// pattern fails to match the way the agent spells the command.
	if m.grants != nil {
		spec.AllowedTools = append(spec.AllowedTools, m.grants.Patterns()...)
	}

	cmd, err := t.adapter.Command(ctx, spec)
	if err != nil {
		fail("building command: " + err.Error())
		return
	}
	cmd.Dir = spec.Cwd
	// The child inherits our environment — that is the point: whatever the host
	// can reach (VPN routes, an SSH agent, credentials) becomes available to the
	// worker with no extra wiring. But some MCP clients launch this server with a
	// curated environment that is missing standard system variables, and the
	// worker inherits the hole too. Restore what is missing first; see
	// agent.RepairedEnviron for why an absent %ProgramData% is not cosmetic.
	env, repaired := agent.RepairedEnviron()
	cmd.Env = env
	guard := newProcGuard(cmd) // cancellation must reach the whole process tree
	defer guard.Close()

	t.audit.Log("turn_start", map[string]any{
		"task_id":      t.ID,
		"agent":        t.AgentName,
		"cwd":          spec.Cwd,
		"prompt":       spec.Prompt,
		"plan_only":    spec.PlanOnly,
		"resume":       spec.SessionID != "",
		"command":      cmd.Args,
		"env_repaired": repaired,
	})

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
	guard.AfterStart(cmd)

	// Safety net: a headless worker can hang forever (e.g. blocked on a
	// permission prompt with no approver). If a timeout is configured, kill the
	// process tree and mark the turn as timed out.
	if m.timeout > 0 {
		timer := time.AfterFunc(m.timeout, func() {
			t.mu.Lock()
			t.timedOut = true
			t.mu.Unlock()
			cancel()
		})
		defer timer.Stop()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go t.pump(stdout, false, sink, &wg)
	go t.pump(stderr, true, sink, &wg)

	// Order matters. Waiting on the pumps first looks natural but hangs forever
	// whenever the agent leaves a background process holding the inherited
	// stdout handle: the pipe never reaches EOF, so the pumps never return, and
	// cmd.Wait — the only thing that honours WaitDelay and force-closes the
	// pipes — is never reached. Calling Wait first lets WaitDelay do its job,
	// after which the pumps see EOF. The grace period below is a second
	// backstop so a stuck reader can never strand the turn.
	pumpsDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(pumpsDone)
	}()
	waitErr := cmd.Wait()
	select {
	case <-pumpsDone:
	case <-time.After(pumpGrace):
	}

	t.mu.Lock()
	t.running = false
	t.cancel = nil
	t.endedAt = time.Now()
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		t.exitCode = &code
	}
	switch {
	case t.timedOut:
		t.status = StatusFailed
		t.runErr = fmt.Sprintf("timed out after %s (the agent may be blocked on a permission prompt with no approver; pre-approve the tool via allowed_tools / CLI_AGENT_MCP_ALLOWED_TOOLS)", m.timeout)
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
	// Not every agent emits a terminal result event. Fall back to something
	// useful so the delegating model always gets an answer, not an empty string.
	if t.resultText == "" {
		if useOutputAsResult(t.adapter) {
			t.resultText = tailLines(t.lines[turnStart:], maxResultLines, maxResultChars)
		} else if t.lastText != "" {
			t.resultText = t.lastText
		}
	}
	endStatus := t.status
	endErr := t.isError
	endResult := t.resultText
	var endExit int
	if t.exitCode != nil {
		endExit = *t.exitCode
	}
	t.mu.Unlock()

	t.persist()

	t.audit.Log("turn_end", map[string]any{
		"task_id":     t.ID,
		"status":      string(endStatus),
		"is_error":    endErr,
		"exit_code":   endExit,
		"duration_ms": time.Since(turnStarted).Milliseconds(),
		"result":      truncateStr(endResult, 500),
	})
}

const (
	maxResultLines = 200
	maxResultChars = 8000

	// A transcript is held in memory for the life of the task, twice over (raw
	// plus rendered). MaxTasks caps how many tasks exist, not how large one can
	// get: a single long run over a big repository could otherwise exhaust the
	// process on its own.
	maxTranscriptLines = 50000
	maxLineBytes       = 64 * 1024

	// How long to wait for the output readers after the process has exited and
	// WaitDelay has already forced the pipes closed.
	pumpGrace = 2 * time.Second
)

func useOutputAsResult(a agent.Adapter) bool {
	r, ok := a.(agent.ResultFromOutput)
	return ok && r.UseOutputAsResult()
}

// truncateStr cuts to at most max bytes without splitting a rune. Naive slicing
// mangles the multi-byte characters this code emits itself (✓ ⚙ ↳ ⚠) and any
// non-ASCII prompt, and json.Marshal then replaces the broken bytes with U+FFFD.
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// truncateTail keeps at most max bytes from the end, on a rune boundary.
func truncateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	start := len(s) - max
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return "…" + s[start:]
}

// tailLines joins the last maxLines lines, capped at maxChars from the end.
func tailLines(lines []string, maxLines, maxChars int) string {
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return truncateTail(strings.TrimSpace(strings.Join(lines, "\n")), maxChars)
}

// appendLine adds a raw line and its compact rendering (which may be ""). The
// two slices stay the same length so a raw line index also indexes its display
// entry. Caller holds t.mu.
func (t *Task) appendLine(raw, display string, isErr bool) {
	if t.truncated {
		return
	}
	if len(t.lines) >= maxTranscriptLines {
		t.truncated = true
		t.lines = append(t.lines, "[transcript truncated: line limit reached]")
		t.display = append(t.display, "⚠ transcript truncated (line limit reached)")
		t.fromStderr = append(t.fromStderr, true)
		return
	}
	line := truncateStr(raw, maxLineBytes)
	t.lines = append(t.lines, line)
	t.display = append(t.display, truncateStr(display, maxLineBytes))
	t.fromStderr = append(t.fromStderr, isErr)

	// Written as it arrives rather than at the end of the turn, so another
	// instance — or this one after a restart — can read a run still in flight.
	if t.store != nil {
		_ = t.store.AppendLine(t.ID, line)
	}
}

// renderEvent produces the compact, human-facing rendering of a parsed event,
// or "" if the line is noise (init/config/rate-limit chatter).
func renderEvent(ev agent.Event) string {
	switch {
	case ev.Final:
		if ev.FinalError {
			return "✗ " + ev.FinalText
		}
		return "✓ " + ev.FinalText
	case ev.ToolName != "":
		if ev.Text != "" {
			return ev.Text
		}
		return "⚙ using " + ev.ToolName
	default:
		return ev.Text
	}
}

// pump reads a stream line by line, parsing agent stdout for structured events.
// Each event is forwarded to sink (if non-nil) after the task lock is released,
// so a streaming caller sees live progress. stderr is surfaced too — when an
// agent fails it often says why only on stderr, and silence there is useless.
//
// Note both pumps share the sink, so a sink must be safe for concurrent use.
func (t *Task) pump(r io.Reader, isErr bool, sink EventSink, wg *sync.WaitGroup) {
	defer wg.Done()
	br := bufio.NewReaderSize(r, 1024*1024)
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			if isErr {
				t.mu.Lock()
				t.appendLine("[stderr] "+line, "⚠ "+strings.TrimSpace(line), true)
				t.mu.Unlock()
				if sink != nil {
					sink(agent.Event{Raw: line, Text: "⚠ " + strings.TrimSpace(line)})
				}
			} else {
				ev := t.adapter.ParseLine(line)
				t.mu.Lock()
				t.appendLine(line, renderEvent(ev), false)
				if ev.SessionID != "" {
					t.sessionID = ev.SessionID
				}
				if ev.Text != "" {
					t.lastText = ev.Text
				}
				if ev.Model != "" {
					t.modelUsed = ev.Model
				}
				if ev.Usage != nil {
					t.usage.Add(*ev.Usage)
				}
				if ev.Final {
					t.isError = ev.FinalError
					if ev.FinalText != "" {
						t.resultText = ev.FinalText
					}
				}
				t.mu.Unlock()
				if ev.ToolName != "" {
					t.audit.Log("tool_use", map[string]any{
						"task_id": t.ID,
						"tool":    ev.ToolName,
						"input":   ev.ToolInput,
					})
				}
				if ev.IsToolResult {
					t.audit.Log("tool_result", map[string]any{
						"task_id":  t.ID,
						"is_error": ev.ToolResultError,
						"output":   truncateStr(strings.TrimPrefix(ev.Text, "↳ "), 500),
					})
				}
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
	t.audit.Log("cancel", map[string]any{"task_id": t.ID})
	if cancel != nil {
		cancel()
	}
	// Wait for the run goroutine to actually transition, rather than guessing at
	// a fixed delay that a loaded machine will outrun.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		still := t.running
		t.mu.Unlock()
		if !still {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return t.Snapshot(), nil
}
