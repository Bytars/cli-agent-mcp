package task

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
)

func parkedManager(t *testing.T) (*Manager, *Task) {
	t.Helper()
	m := NewManager(10)
	tk := &Task{ID: "t1", status: StatusRunning, running: true, adapter: agent.NewMockAdapter()}
	m.tasks = map[string]*Task{"t1": tk}
	m.order = []string{"t1"}
	return m, tk
}

// The whole point: a worker that needs something it was not given holds instead
// of failing, and a person releases it.
func TestAskPermissionWaitsForAnAnswer(t *testing.T) {
	m, tk := parkedManager(t)

	answered := make(chan PermissionAnswer, 1)
	go func() {
		answered <- m.AskPermission(context.Background(), "t1", "PowerShell",
			"docker compose up -d", "docker", 10*time.Second)
	}()

	// The request has to become visible without anyone asking for it by id.
	deadline := time.Now().Add(3 * time.Second)
	var pending []PermissionRequest
	for time.Now().Before(deadline) {
		if pending = m.PendingPermissions(); len(pending) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatalf("the request never showed up as pending: %+v", pending)
	}
	if pending[0].Tool != "PowerShell" || pending[0].Command != "docker" {
		t.Errorf("pending request = %+v, want the tool and command that were asked about", pending[0])
	}

	// And on the task itself, which is what a watcher reads.
	if snap := tk.Snapshot(); snap.Pending == nil {
		t.Error("the task's snapshot does not report that it is blocked")
	}
	// The transcript is how it reaches agent_watch and the board.
	if _, _, _, text := tk.Output(0, 0, true); !strings.Contains(text, "WAITING FOR PERMISSION") {
		t.Errorf("nothing in the transcript says the task is blocked:\n%s", text)
	}

	if _, err := m.AnswerPermission("t1", "", PermissionAnswer{Allow: true}); err != nil {
		t.Fatalf("answering: %v", err)
	}

	select {
	case ans := <-answered:
		if !ans.Allow {
			t.Error("the worker was refused after the answer said allow")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the worker was never released")
	}

	if snap := tk.Snapshot(); snap.Pending != nil {
		t.Error("the task still reports a pending request after it was answered")
	}
}

// A refusal has to carry a reason: the worker can route around "you may not
// push" but not around silence.
func TestDenialReachesTheAgentWithAReason(t *testing.T) {
	m, _ := parkedManager(t)

	answered := make(chan PermissionAnswer, 1)
	go func() {
		answered <- m.AskPermission(context.Background(), "t1", "Bash", "git push", "git", 10*time.Second)
	}()
	waitForPending(t, m)

	if _, err := m.AnswerPermission("t1", "", PermissionAnswer{Message: "we never push from here"}); err != nil {
		t.Fatalf("answering: %v", err)
	}
	ans := <-answered
	if ans.Allow {
		t.Fatal("a denial was reported as an allow")
	}
	if !strings.Contains(ans.Message, "never push") {
		t.Errorf("message = %q, want the reason passed through", ans.Message)
	}
}

// An unattended run must not wait forever, and the worker has to be told why it
// was refused so it can finish the rest of the task.
func TestUnansweredRequestTimesOutAsARefusal(t *testing.T) {
	m, tk := parkedManager(t)

	start := time.Now()
	ans := m.AskPermission(context.Background(), "t1", "Bash", "rm -rf /", "rm", 300*time.Millisecond)
	if ans.Allow {
		t.Fatal("an unanswered request was allowed")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("waited %s on a 300ms timeout", elapsed.Round(time.Millisecond))
	}
	if !strings.Contains(ans.Message, "nobody answered") {
		t.Errorf("message = %q, want it to say nobody answered", ans.Message)
	}
	if _, _, _, text := tk.Output(0, 0, true); !strings.Contains(text, "timed out") {
		t.Errorf("the transcript does not record the timeout:\n%s", text)
	}
	if snap := tk.Snapshot(); snap.Pending != nil {
		t.Error("a timed-out request is still reported as pending")
	}
}

func TestAnsweringSomethingNobodyAskedIsAnError(t *testing.T) {
	m, _ := parkedManager(t)
	if _, err := m.AnswerPermission("t1", "", PermissionAnswer{Allow: true}); err == nil {
		t.Error("expected an error when there is no request waiting")
	}
}

func waitForPending(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.PendingPermissions()) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no request became pending")
}
