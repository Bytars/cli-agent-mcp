// SPDX-License-Identifier: Apache-2.0

package task

import (
	"context"
	"testing"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
	"github.com/Bytars/cli-agent-mcp/internal/state"
)

// A second server instance can see a task the first one owns — the record and
// transcript are on disk and keep advancing — but it holds no handle on the
// worker, so it could not stop one. Worse, asking it to used to return the
// snapshot as though it had: the caller read success while the task carried on.
//
// The request is now left where the owning instance will find it.
func TestASecondInstanceCanStopTheFirstsTask(t *testing.T) {
	t.Setenv(helperEnv, "1")

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer store.Close()

	owner := NewManager(10)
	owner.SetStore(store)

	tk, err := owner.StartTask(sleepAdapter{ms: 20000}, t.TempDir(), agent.RunSpec{Prompt: "work"}, Options{})
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	waitFor(t, 5*time.Second, "the owner's task to start running", func() bool {
		return tk.Snapshot().Status == StatusRunning
	})

	// The second instance comes up against the same directory and restores it.
	other := NewManager(10)
	other.SetStore(store)
	if n := other.Restore(agent.NewRegistry(agent.NewMockAdapter())); n != 1 {
		t.Fatalf("the second instance restored %d task(s), want 1", n)
	}
	seen, ok := other.Get(tk.ID)
	if !ok {
		t.Fatal("the second instance cannot see the task at all")
	}
	if s := seen.Snapshot().Status; s != StatusOrphaned {
		t.Fatalf("restored status = %q, want %q", s, StatusOrphaned)
	}

	if _, err := other.Cancel(tk.ID); err != nil {
		t.Fatalf("cancelling from the second instance: %v", err)
	}

	// The proof: the OWNER's worker actually stops, without the second instance
	// ever touching the process.
	waitFor(t, 15*time.Second, "the owner's worker to stop", func() bool {
		return tk.Snapshot().Status != StatusRunning
	})
	if s := tk.Snapshot().Status; s != StatusCanceled {
		t.Errorf("owner's task ended as %q, want %q", s, StatusCanceled)
	}

	// The request must not outlive its use, or a task id reused by a later run
	// would be stopped the moment it started.
	if store.CancelRequested(tk.ID) {
		t.Error("the cancel request was left behind after being acted on")
	}
}

// A request left over from an earlier run must not stop the next task to carry
// that id. The window is small but the failure it causes — a task that dies
// instantly for no visible reason — is the kind nobody diagnoses quickly.
func TestAStaleRequestDoesNotStopTheNextRun(t *testing.T) {
	t.Setenv(helperEnv, "1")

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer store.Close()

	m := NewManager(10)
	m.SetStore(store)

	tk := m.newTask(sleepAdapter{ms: 1500}, t.TempDir(), agent.RunSpec{Prompt: "work"})
	if err := store.RequestCancel(tk.ID); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if err := m.admit(tk); err != nil {
		t.Fatalf("admit: %v", err)
	}
	go m.runTurn(context.Background(), tk, agent.RunSpec{Prompt: "work", Cwd: t.TempDir()}, nil)

	waitFor(t, 15*time.Second, "the task to finish", func() bool {
		return tk.Snapshot().Status != StatusRunning
	})
	if s := tk.Snapshot().Status; s != StatusDone {
		t.Errorf("status = %q, want %q — a stale request from a previous run stopped it", s, StatusDone)
	}
}

func waitFor(t *testing.T, limit time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}
