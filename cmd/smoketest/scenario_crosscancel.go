// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This scenario runs two servers, and the whole proof rests on both reading the
// same state directory. The harness's own server is spawned in main before any
// scenario runs, inheriting its environment from this process, so nothing in
// here can reach back and set CLI_AGENT_MCP_STATE_DIR for it. It therefore has
// to come from the operator, and runCrossCancel refuses to run without it:
//
//	CLI_AGENT_MCP_STATE_DIR=<dir> SMOKE_ONLY=crosscancel SMOKE_AGENT=mock \
//	    go run ./cmd/smoketest ./cli-agent-mcp.exe
//
// Required rather than defaulted, because both fallbacks are worse than an
// error. Letting it fall through to the per-user directory would run this proof
// in amongst the operator's real tasks, and cancel it. And relying on the two
// instances happening to agree on that default would make the scenario fail, on
// a machine configured differently, for a reason that has nothing to do with
// the behaviour under test.

func init() {
	register("crosscancel", scenario{
		// Needs is empty on purpose: the mechanism being proved is entirely
		// server-side — a request file one instance writes and another picks up
		// — and does not depend on what the worker is or what it does. The mock
		// is simply the cheapest worker to keep busy for twenty seconds.
		Needs: "",
		What: "agent_cancel_task on a task owned by ANOTHER cli-agent-mcp process actually stops it.\n" +
			"The unit test proves this with two managers in one process. This proves it across two OS\n" +
			"processes, which is the situation that actually occurs: a client starts a second server\n" +
			"alongside the first, and from there the first instance's tasks are visible but untouchable.\n" +
			"Requires CLI_AGENT_MCP_STATE_DIR to be set, so both instances share a state directory.",
		Run: runCrossCancel,
	})
}

const (
	// How long to wait for the task's record to reach disk. A second instance
	// restores from those files at startup, so there is nothing to see before.
	xcancelRecordWindow = 15 * time.Second

	// How long the owning instance gets to notice the request and act on it. It
	// polls once a second, so this is generous by an order of magnitude; the
	// point of the bound is to fail with an explanation rather than hang.
	xcancelActWindow = 15 * time.Second
)

// runCrossCancel proves that a cancel asked for in one process stops a worker
// owned by another.
//
// Before this, the second instance answered the request with the task snapshot,
// which reads as success — so the caller believed the task was stopping while
// the worker carried on spending. It could not do better: it holds no handle on
// a process it did not spawn, and killing by the recorded pid is not safe when
// pids are recycled. Leaving the request where the owner will find it is.
func runCrossCancel(ctx context.Context, e *env) {
	stateDir := xcancelStateDir()
	fmt.Printf("  shared state directory: %s\n", stateDir)

	// 1. A task on the harness's server — instance A, the one that owns the
	//    worker. It has to still be running when the assertions below land, so
	//    twenty seconds against a mock that does nothing to the working tree.
	prompt := "sleep:20000"
	st := callTool(ctx, e.Session, "agent_start_task", map[string]any{
		"prompt": prompt,
		"agent":  e.Agent,
		"cwd":    e.Cwd,
	})
	id := jsonField(st, "task_id")
	if id == "" {
		log.Fatalf("FAIL: agent_start_task returned no task_id:\n%s", indent(textContent(st)))
	}
	if s := jsonField(st, "status"); s != "running" {
		log.Fatalf("FAIL: the task must be live for any of this to mean anything, but instance A reports %q:\n%s",
			s, indent(textContent(st)))
	}
	fmt.Printf("  instance A started %s (would run ~20s)\n", id)

	// Leave no worker behind if an assertion below stops us early. (log.Fatalf
	// skips this, but it also ends the process, which kills the worker with it.)
	defer func() {
		_, _ = e.Session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "agent_cancel_task",
			Arguments: map[string]any{"task_id": id},
		})
	}()

	// The second instance restores what it finds on disk at startup, so bringing
	// it up before the record lands would leave it blind to the task through no
	// fault of the mechanism.
	xcancelAwaitRecord(ctx, stateDir, id)

	// 2. A second server process against the same directory — instance B.
	second := xcancelSpawn(ctx, e, stateDir)
	defer func() {
		// Closing the session closes the server's stdin and reaps the process,
		// so nothing is left holding the state directory.
		if err := second.Close(); err != nil {
			fmt.Printf("  (second instance did not shut down cleanly: %v)\n", err)
		}
	}()

	// 3. Instance B can see the task, and is honest that it is not its own.
	seen, err := second.CallTool(ctx, &mcp.CallToolParams{
		Name:      "agent_task_status",
		Arguments: map[string]any{"task_id": id},
	})
	if err != nil {
		log.Fatalf("agent_task_status on the second instance: %v", err)
	}
	if seen.IsError {
		log.Fatalf("FAIL: the second instance cannot see task %s at all, so there is nothing for it to cancel. "+
			"Both instances must be reading %s:\n%s", id, stateDir, indent(textContent(seen)))
	}
	if s := jsonField(seen, "status"); s != "orphaned" {
		log.Fatalf("FAIL: the second instance reports %s as %q, want \"orphaned\" — it does not own this worker "+
			"and must not present it as something it can watch:\n%s", id, s, indent(textContent(seen)))
	}
	fmt.Printf("  instance B sees %s as orphaned\n", id)

	// 4. Ask instance B to stop it. Not callTool: the answer is the assertion,
	//    and a tool error here would be swallowed as a fatal with no context.
	asked := time.Now()
	cancelRes, err := second.CallTool(ctx, &mcp.CallToolParams{
		Name:      "agent_cancel_task",
		Arguments: map[string]any{"task_id": id},
	})
	if err != nil {
		log.Fatalf("agent_cancel_task on the second instance: %v", err)
	}
	if cancelRes.IsError {
		log.Fatalf("FAIL: the second instance refused to cancel %s outright:\n%s", id, indent(textContent(cancelRes)))
	}
	said := textContent(cancelRes)
	// Fragments, not the whole sentence: the wording will be rewritten, but a
	// reply that stops mentioning the other instance or the request left for it
	// has stopped telling the caller what actually happened.
	for _, want := range []string{"another cli-agent-mcp instance", "left for it"} {
		if !strings.Contains(said, want) {
			log.Fatalf("FAIL: the reply does not say the request was left for the owning instance (no %q), "+
				"so a caller cannot tell this from a cancel that already took effect:\n%s",
				want, indent(said))
		}
	}
	// The old answer, and the one that must never come back: the bare snapshot,
	// which reads as though the cancel had happened.
	if strings.Contains(said, "status=orphaned") {
		log.Fatalf("FAIL: the reply is the plain snapshot line, which reads as success while the worker carries on:\n%s",
			indent(said))
	}
	fmt.Printf("  instance B answered: %s\n", firstLine(said))

	// 5. The proof. Instance A owns the worker and nothing has touched its
	//    process; if the task stops, it is because A picked the request up.
	status := xcancelAwaitStopped(ctx, e, id, asked)

	// 6. The request must not outlive its use. Left lying there it would stop
	//    the next task to carry this id, instantly and for no visible reason.
	//    Read as a plain file rather than through internal/state: this scenario
	//    is an outside observer of the server, and importing the package that
	//    writes the file would let a rename break both halves in step.
	pending := xcancelTaskFile(stateDir, id, ".cancel")
	if _, err := os.Stat(pending); err == nil {
		log.Fatalf("FAIL: %s survived being acted on; the next task with this id would be killed the moment it started", pending)
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("FAIL: stat %s: %v", pending, err)
	}

	fmt.Printf("CROSSCANCEL DONE (a cancel asked for in one process stopped a worker owned by another; status=%s)\n", status)
}

// xcancelStateDir reads the directory both instances must share, and refuses to
// guess one. See the note at the top of the file for why it cannot be defaulted.
func xcancelStateDir() string {
	dir := strings.TrimSpace(os.Getenv("CLI_AGENT_MCP_STATE_DIR"))
	if dir == "" {
		example := filepath.Join(os.TempDir(), "cli-agent-mcp-crosscancel")
		log.Fatalf("FAIL: crosscancel needs CLI_AGENT_MCP_STATE_DIR set in the environment that launched this harness. "+
			"It runs two servers and they must share a state directory, but the one this harness drives was started "+
			"before any scenario ran and inherited its environment from here, so it cannot be set from inside. Re-run:\n\n"+
			"    CLI_AGENT_MCP_STATE_DIR=%s SMOKE_ONLY=crosscancel SMOKE_AGENT=mock go run ./cmd/smoketest %s\n",
			example, xcancelServerExe())
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		log.Fatalf("FAIL: CLI_AGENT_MCP_STATE_DIR=%q is not a usable path: %v", dir, err)
	}
	return abs
}

// xcancelServerExe repeats the choice main makes for the harness's own server.
// The second instance has to be the same binary, or the two are not the pair
// this scenario is about.
func xcancelServerExe() string {
	if len(os.Args) > 1 {
		return os.Args[1]
	}
	return "./cli-agent-mcp.exe"
}

// xcancelTaskFile names one of a task's files in the store's layout.
func xcancelTaskFile(stateDir, id, ext string) string {
	return filepath.Join(stateDir, "tasks", id+ext)
}

// xcancelAwaitRecord waits for the task's record to be written, because that
// file is all a second instance has to restore from.
func xcancelAwaitRecord(ctx context.Context, stateDir, id string) {
	path := xcancelTaskFile(stateDir, id, ".json")
	deadline := time.Now().Add(xcancelRecordWindow)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			log.Fatalf("FAIL: %s never appeared within %s, so a second instance would come up unable to see the task "+
				"at all. Check that the harness's server is using this state directory.", path, xcancelRecordWindow)
		}
		if err := ctx.Err(); err != nil {
			log.Fatalf("FAIL: scenario context ended while waiting for the task record: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// xcancelSpawn brings up the second server and connects a client to it, the
// same way main does for the first.
func xcancelSpawn(ctx context.Context, e *env, stateDir string) *mcp.ClientSession {
	exe := xcancelServerExe()
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"CLI_AGENT_MCP_DEFAULT_AGENT="+e.Agent,
		// Set explicitly even though it is already in the environment: the whole
		// scenario rests on the two instances reading one directory, and that is
		// worth stating here rather than inheriting silently.
		"CLI_AGENT_MCP_STATE_DIR="+stateDir,
	)
	// Its startup log says whether it found the first instance's lock, which is
	// the first thing to look at when this scenario fails.
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "smoketest-second-instance", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		log.Fatalf("FAIL: could not start a second server from %s: %v", exe, err)
	}
	fmt.Printf("  second instance up (%s, pid %d)\n", exe, cmd.Process.Pid)
	return session
}

// xcancelAwaitStopped polls the instance that OWNS the worker until the task
// settles, and reports how long the request took to be acted on.
func xcancelAwaitStopped(ctx context.Context, e *env, id string, asked time.Time) string {
	deadline := asked.Add(xcancelActWindow)
	for {
		res := callTool(ctx, e.Session, "agent_task_status", map[string]any{"task_id": id})
		status := jsonField(res, "status")
		if status != "running" {
			took := time.Since(asked).Round(10 * time.Millisecond)
			fmt.Printf("  instance A acted on the request after %v: status=%s\n", took, status)
			if status != "canceled" {
				log.Fatalf("FAIL: the owning instance ended %s as %q, want \"canceled\" (error=%q):\n%s",
					id, status, jsonField(res, "error"), indent(textContent(res)))
			}
			return status
		}
		if time.Now().After(deadline) {
			log.Fatalf("FAIL: %s was still running on the instance that owns it %s after another instance asked for it "+
				"to stop. The request was written but never picked up, so the caller was told one thing and the worker "+
				"did another:\n%s", id, xcancelActWindow, indent(textContent(res)))
		}
		if err := ctx.Err(); err != nil {
			log.Fatalf("FAIL: scenario context ended while waiting for %s to stop: %v", id, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
