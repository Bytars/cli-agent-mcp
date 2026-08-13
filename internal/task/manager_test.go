package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
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
