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

	tk, err := owner.StartTask(sleepAdapter{ms: 20000}, At(t.TempDir()), agent.RunSpec{Prompt: "work"}, Options{})
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	// If anything below fails, the worker would otherwise sit out its twenty
	// seconds holding the temp directory open, and Windows reports that as a
	// second failure on top of the real one.
	t.Cleanup(func() { _, _ = owner.Cancel(tk.ID) })
	waitFor(t, 5*time.Second, "the owner's task to start running", func() bool {
		return tk.Snapshot().Status == StatusRunning
	})

	// The second instance comes up against the same directory and restores it.
	other := NewManager(10)
	other.SetStore(store)
	// nil: este test monta a propósito el caso en que la otra instancia NO está
	// viva, para poder ejercitar el mecanismo de cancelación cruzada a nivel
	// Manager. Con un dueño vivo, Restore no adoptaría nada (issue #21) y no
	// habría tarea sobre la cual probar nada.
	if n := other.Restore(agent.NewRegistry(agent.NewMockAdapter()), nil); n != 1 {
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

// A request can be written before the turn's watcher is even scheduled: the
// caller doing the writing waits on nothing, while the turn's goroutine is busy
// starting a process. Such a request must still be honoured.
//
// This is not hypothetical. The watcher used to clear anything it found on
// entry, as a precaution against a leftover from a task with the same id — a
// situation that cannot really arise, since ids carry a random suffix and Forget
// removes the request with the record. What the precaution did instead was
// swallow real requests that landed in that window, which passed on a fast
// machine and failed on the first loaded one.
func TestARequestArrivingBeforeTheWatcherIsHonoured(t *testing.T) {
	t.Setenv(helperEnv, "1")

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer store.Close()

	m := NewManager(10)
	m.SetStore(store)

	tk := m.newTask(sleepAdapter{ms: 20000}, At(t.TempDir()), agent.RunSpec{Prompt: "work"})
	if err := m.admit(tk); err != nil {
		t.Fatalf("admit: %v", err)
	}

	// Written before the turn starts at all, which is the worst case for the
	// window this guards.
	if err := store.RequestCancel(tk.ID); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	go m.runTurn(context.Background(), tk, agent.RunSpec{Prompt: "work", Cwd: t.TempDir()}, nil)
	t.Cleanup(func() { _, _ = m.Cancel(tk.ID) })

	waitFor(t, 20*time.Second, "the turn to act on a request that preceded it", func() bool {
		return tk.Snapshot().Status != StatusRunning
	})
	if s := tk.Snapshot().Status; s != StatusCanceled {
		t.Errorf("status = %q, want %q — the request was written first and must not have been discarded", s, StatusCanceled)
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
