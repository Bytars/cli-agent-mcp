// SPDX-License-Identifier: Apache-2.0

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

	"github.com/Bytars/cli-agent-mcp/internal/agent"
)

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

	tk, finished, err := m.RunTaskStreaming(ctx, sleepAdapter{ms: 1500}, t.TempDir(),
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
	tk, finished, err := m.RunTaskStreaming(context.Background(), sleepAdapter{ms: 3000}, t.TempDir(),
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
		tk, err := m.StartTask(sleepAdapter{ms: 2000}, t.TempDir(), agent.RunSpec{Prompt: "work"}, Options{})
		if err != nil {
			t.Fatalf("task %d should have been admitted: %v", i, err)
		}
		started = append(started, tk)
	}

	if _, err := m.StartTask(sleepAdapter{ms: 2000}, t.TempDir(), agent.RunSpec{Prompt: "work"}, Options{}); err == nil {
		t.Fatal("a third concurrent task must be refused")
	} else if !strings.Contains(err.Error(), "MAX_CONCURRENT") {
		t.Errorf("the refusal must name the setting that caused it, got: %v", err)
	}

	for _, tk := range started {
		_, _ = m.Cancel(tk.ID)
	}
}

// A cap of zero is the documented way to turn the limit off, and it must not be
// read as "admit nothing".
func TestZeroConcurrencyLimitMeansNoLimit(t *testing.T) {
	t.Setenv(helperEnv, "1")
	m := NewManager(10)
	m.SetMaxConcurrent(0)

	var started []*Task
	for i := 0; i < 4; i++ {
		tk, err := m.StartTask(sleepAdapter{ms: 1500}, t.TempDir(), agent.RunSpec{Prompt: "work"}, Options{})
		if err != nil {
			t.Fatalf("task %d was refused although the limit is disabled: %v", i, err)
		}
		started = append(started, tk)
	}
	for _, tk := range started {
		_, _ = m.Cancel(tk.ID)
	}
}
