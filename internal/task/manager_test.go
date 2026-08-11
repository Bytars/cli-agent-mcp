package task

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andresh0816/cli-agent-mcp/internal/agent"
)

func TestTruncateStrKeepsRunesIntact(t *testing.T) {
	// The compact renderer emits ✓ ⚙ ↳ ⚠ (3 bytes each) and prompts here are
	// routinely non-ASCII. Byte slicing splits them and json.Marshal then
	// replaces the fragments with U+FFFD.
	s := strings.Repeat("é", 50) // 100 bytes
	for _, max := range []int{1, 5, 33, 99} {
		got := truncateStr(s, max)
		body := strings.TrimSuffix(got, "…")
		if !isValidUTF8(body) {
			t.Errorf("truncateStr(max=%d) produced invalid UTF-8: %q", max, body)
		}
		if len(body) > max {
			t.Errorf("truncateStr(max=%d) returned %d bytes", max, len(body))
		}
	}
}

func TestTruncateTailKeepsRunesIntact(t *testing.T) {
	s := strings.Repeat("ñ", 40)
	got := truncateTail(s, 15)
	body := strings.TrimPrefix(got, "…")
	if !isValidUTF8(body) {
		t.Errorf("truncateTail produced invalid UTF-8: %q", body)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

func TestAppendLineBoundsTheTranscript(t *testing.T) {
	tk := &Task{}
	for i := 0; i < maxTranscriptLines+500; i++ {
		tk.appendLine("line", "line", false)
	}
	if len(tk.lines) > maxTranscriptLines+1 {
		t.Errorf("transcript grew to %d lines; the cap is %d (+1 marker)", len(tk.lines), maxTranscriptLines)
	}
	if !tk.truncated {
		t.Error("expected the task to be marked truncated")
	}
	if !strings.Contains(tk.lines[len(tk.lines)-1], "truncated") {
		t.Error("expected a truncation marker as the final line")
	}
}

func TestAppendLineBoundsLineLength(t *testing.T) {
	tk := &Task{}
	tk.appendLine(strings.Repeat("x", maxLineBytes*3), "d", false)
	if len(tk.lines[0]) > maxLineBytes+4 {
		t.Errorf("a single line kept %d bytes; the cap is %d", len(tk.lines[0]), maxLineBytes)
	}
}

func TestRawOutputExcludesStderr(t *testing.T) {
	// raw:true is documented as the agent's JSONL transcript. stderr lines are
	// not JSON and would break any client parsing the result.
	tk := &Task{}
	tk.appendLine(`{"type":"a"}`, "a", false)
	tk.appendLine("[stderr] warning: something", "⚠ warning", true)
	tk.appendLine(`{"type":"b"}`, "b", false)

	_, _, _, raw := tk.Output(0, 0, false)
	if strings.Contains(raw, "[stderr]") {
		t.Errorf("raw output must not contain stderr lines:\n%s", raw)
	}
	for _, want := range []string{`{"type":"a"}`, `{"type":"b"}`} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw output lost %s:\n%s", want, raw)
		}
	}

	_, _, _, compact := tk.Output(0, 0, true)
	if !strings.Contains(compact, "warning") {
		t.Error("compact output must still surface stderr")
	}
}

// agent_watch bounds how long it blocks so it can return before the client
// gives up on the call. That bound is only real if WatchFrom honours its
// deadline on a task producing nothing: if it blocked past it, the watch would
// still be cut off mid-call and its result thrown away.
func TestWatchFromHonoursItsDeadlineWhileStillRunning(t *testing.T) {
	tk := &Task{ID: "t1", status: StatusRunning, running: true}

	start := time.Now()
	text, _, _, status, running := tk.WatchFrom(context.Background(), 0, 200*time.Millisecond, true)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("blocked %s on a 200ms deadline", elapsed.Round(time.Millisecond))
	}
	if !running || status != StatusRunning {
		t.Errorf("running=%v status=%q, want the task reported as still going", running, status)
	}
	if text != "" {
		t.Errorf("got output %q from a task that produced none", text)
	}
}

// helperEnv marks a re-invocation of the test binary as the fake agent below.
const helperEnv = "CLI_AGENT_MCP_TEST_HELPER"

// TestHelperProcess is the child process that sleepAdapter spawns: it stands in
// for a headless agent that takes a while and then exits cleanly. It does
// nothing at all during a normal test run.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	ms := 0
	for i, a := range os.Args {
		if a == "--sleep-ms" && i+1 < len(os.Args) {
			ms, _ = strconv.Atoi(os.Args[i+1])
		}
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	fmt.Println(`{"type":"result","subtype":"success","session_id":"helper-1","is_error":false,"result":"done"}`)
	os.Exit(0)
}

// sleepAdapter runs the helper process above as if it were a CLI agent.
type sleepAdapter struct{ ms int }

func (a sleepAdapter) Name() string              { return "sleeper" }
func (a sleepAdapter) Available() (bool, string) { return true, "test helper" }

func (a sleepAdapter) Command(ctx context.Context, spec agent.RunSpec) (*exec.Cmd, error) {
	// The bare "--" stops the testing package from parsing what follows as its
	// own flags; without it the child exits 2 before running anything.
	return exec.CommandContext(ctx, os.Args[0],
		"-test.run=TestHelperProcess", "--", "--sleep-ms", strconv.Itoa(a.ms)), nil
}

func (a sleepAdapter) ParseLine(line string) agent.Event {
	return agent.Event{Raw: line}
}

// A delegated task has to outlive the tool call that asked for it. Tying the
// worker to the request context meant that when the client gave up on the call
// — which Claude Desktop does after 60s — the worker was killed mid-edit and
// the task was recorded as "failed" with an exit code, indistinguishable from
// the agent genuinely failing.
func TestRunTaskStreamingSurvivesRequestCancellation(t *testing.T) {
	t.Setenv(helperEnv, "1")
	m := NewManager(10)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	tk, finished, err := m.RunTaskStreaming(ctx, sleepAdapter{ms: 1500}, At(t.TempDir()),
		agent.RunSpec{Prompt: "work"}, Options{Window: 30 * time.Second})
	if err != nil {
		t.Fatalf("RunTaskStreaming: %v", err)
	}
	if finished {
		t.Fatal("the call returned finished=true even though the request was cancelled first")
	}

	// The point of the fix: the worker is still going.
	if snap := tk.Snapshot(); snap.Status != StatusRunning {
		t.Fatalf("status = %q right after the request was abandoned, want %q", snap.Status, StatusRunning)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if snap := tk.Snapshot(); snap.Status != StatusRunning {
			if snap.Status != StatusDone {
				t.Fatalf("task ended as %q (exit=%v, err=%q), want it to complete normally",
					snap.Status, snap.ExitCode, snap.Error)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the task never finished after the request was abandoned")
}

// The window exists so a blocking call returns before the client abandons it.
func TestRunTaskStreamingReturnsWhenTheWindowCloses(t *testing.T) {
	t.Setenv(helperEnv, "1")
	m := NewManager(10)

	start := time.Now()
	tk, finished, err := m.RunTaskStreaming(context.Background(), sleepAdapter{ms: 3000}, At(t.TempDir()),
		agent.RunSpec{Prompt: "work"}, Options{Window: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("RunTaskStreaming: %v", err)
	}
	if finished {
		t.Fatal("expected the call to return before the turn finished")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("blocked %s on a 300ms window", elapsed.Round(time.Millisecond))
	}
	if snap := tk.Snapshot(); snap.Status != StatusRunning {
		t.Errorf("status = %q, want the task still running", snap.Status)
	}
	if _, err := m.Cancel(tk.ID); err != nil {
		t.Errorf("cancelling the still-running task: %v", err)
	}
}

// MaxTasks bounds retained records, not live workers. Without a separate cap an
// orchestrator can fan out enough quarter-gigabyte agent processes to take the
// machine down while every other limit still looks satisfied.
func TestConcurrencyLimitRefusesExtraWorkers(t *testing.T) {
	t.Setenv(helperEnv, "1")
	m := NewManager(10)
	m.SetMaxConcurrent(2)

	var started []*Task
	for i := 0; i < 2; i++ {
		tk, err := m.StartTask(sleepAdapter{ms: 2000}, At(t.TempDir()), agent.RunSpec{Prompt: "work"}, Options{})
		if err != nil {
			t.Fatalf("task %d should have been admitted: %v", i, err)
		}
		started = append(started, tk)
	}

	if _, err := m.StartTask(sleepAdapter{ms: 2000}, At(t.TempDir()), agent.RunSpec{Prompt: "work"}, Options{}); err == nil {
		t.Fatal("a third concurrent task must be refused")
	} else if !strings.Contains(err.Error(), "MAX_CONCURRENT") {
		t.Errorf("the refusal must name the setting that caused it, got: %v", err)
	}

	for _, tk := range started {
		_, _ = m.Cancel(tk.ID)
	}
}

func TestPrepareFollowupClaimsTaskUnderLock(t *testing.T) {
	// Two concurrent follow-ups used to both pass the `running` check and spawn
	// against the same session. The claim must happen before the lock is
	// released, so the second attempt is rejected.
	m := NewManager(10)
	// The adapter matters only because prepareFollowup now refuses to resume a
	// task whose agent is gone; every real task has one.
	tk := &Task{ID: "t1", status: StatusDone, sessionID: "sess-1", adapter: agent.NewMockAdapter()}
	m.tasks = map[string]*Task{"t1": tk}
	m.order = []string{"t1"}

	if _, _, err := m.prepareFollowup("t1", "primero", nil, nil); err != nil {
		t.Fatalf("first follow-up should be accepted: %v", err)
	}
	if _, _, err := m.prepareFollowup("t1", "segundo", nil, nil); err == nil {
		t.Fatal("second concurrent follow-up must be rejected, but it was accepted")
	}
}
