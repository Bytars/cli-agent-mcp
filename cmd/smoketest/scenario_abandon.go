// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	register("abandon", scenario{
		Needs: "real",
		What: "a delegated turn outlives the MCP call that asked for it.\n" +
			"The client drops agent_run_task while the worker is mid-task, and the worker\n" +
			"still runs the rest of its work to completion and lands it on disk.\n" +
			"The regression this guards: the turn used to run on the request's context, so a\n" +
			"client that stopped waiting killed the worker mid-edit and the task was recorded\n" +
			"as status=failed error=\"context canceled\" — indistinguishable from a real failure.",
		Run: runAbandon,
	})
}

// Phase budgets. The delegated task is deliberately slow — it has to still be
// under way when the call is dropped — so finishing gets the largest one.
const (
	abandonStartWindow = 45 * time.Second  // for the run to register and start producing
	abandonCallWindow  = 30 * time.Second  // for the dropped call to report its error
	abandonDoneWindow  = 150 * time.Second // for the detached turn to finish

	// How many read-then-write links the worker must walk. Each link is two
	// tool calls it cannot skip, which is what buys the runtime; five puts the
	// turn around 20-40s against Claude Code, comfortably longer than the few
	// seconds it takes this scenario to drop the call.
	abandonChainLinks = 5
)

// abandonTaskRow is the slice of an agent_list_tasks entry this scenario reads.
// jsonField only reaches top-level scalars, and the task has to be identified by
// a nested prompt.
type abandonTaskRow struct {
	ID      string   `json:"task_id"`
	Prompts []string `json:"prompts"`
}

func runAbandon(ctx context.Context, e *env) {
	// A marker unique to this run keeps every assertion honest: it picks this
	// task out of whatever else the server is holding, and no file left behind
	// by an earlier run can satisfy the disk check at the end.
	marker := fmt.Sprintf("abandon-%d", time.Now().UnixNano())
	prompt, want := abandonChain(marker)
	last := abandonLink(marker, abandonChainLinks)
	path := filepath.Join(e.Cwd, last)

	// The per-call context is the whole experiment: cancelling it drops this one
	// MCP request exactly as a client giving up would drop it. It must not be
	// the scenario's ctx, which still has the rest of the proof to carry out.
	callCtx, abandonCall := context.WithCancel(ctx)
	defer abandonCall()

	params := &mcp.CallToolParams{Name: "agent_run_task", Arguments: map[string]any{
		"prompt": prompt,
		"agent":  e.Agent,
		"cwd":    e.Cwd,
		// Pre-approve, because nobody will be listening: once the call is
		// abandoned there is no one on the other end to answer a permission
		// request, and the worker would sit blocked until the desk times out.
		"allowed_tools": []string{"Bash", "PowerShell", "Write", "Edit", "Read"},
	}}
	params.SetProgressToken("abandon-1")

	type callOutcome struct {
		res *mcp.CallToolResult
		err error
	}
	outcome := make(chan callOutcome, 1)
	before := len(e.Progress())
	go func() {
		// Deliberately not callTool: this call is expected to fail, and its
		// error is the assertion.
		res, err := e.Session.CallTool(callCtx, params)
		outcome <- callOutcome{res, err}
	}()

	fmt.Printf("  delegated a %d-link chain ending in %s\n", abandonChainLinks, last)
	taskID := abandonFindTask(ctx, e, marker)
	abandonAwaitOutput(ctx, e, before)
	fmt.Printf("  task %s is under way — dropping the call that asked for it\n", taskID)
	abandonCall()

	// 1. The call really was abandoned. Without this the rest proves nothing: a
	//    call that returned normally was never dropped.
	var got callOutcome
	select {
	case got = <-outcome:
	case <-time.After(abandonCallWindow):
		log.Fatalf("FAIL: agent_run_task did not return within %s of its context being cancelled; "+
			"the client-side abandon never took effect", abandonCallWindow)
	}
	if got.err == nil {
		log.Fatalf("FAIL: agent_run_task returned normally, so the call was never abandoned "+
			"(the agent finished the whole chain first). Nothing was proved:\n%s",
			indent(textContent(got.res)))
	}
	if !errors.Is(got.err, context.Canceled) {
		log.Fatalf("FAIL: expected agent_run_task to fail with a context error, got: %v", got.err)
	}
	fmt.Printf("  call abandoned: %v\n", got.err)

	// 2. The worker survived it. This is the fix: on a fresh context the task is
	//    still running or already finished — never killed by the dropped call.
	st := callTool(ctx, e.Session, "agent_task_status", map[string]any{"task_id": taskID})
	switch s := jsonField(st, "status"); s {
	case "running", "done":
		fmt.Printf("  status right after the abandon: %s\n", s)
	default:
		log.Fatalf("FAIL: the abandoned call took the worker down with it — status=%q error=%q. "+
			"The turn must run on a context of its own, not on the MCP request's:\n%s",
			s, jsonField(st, "error"), indent(textContent(st)))
	}

	// 3. The end of the chain had not been reached yet, so what lands there is
	//    work performed with nobody waiting on it.
	if _, err := os.Stat(path); err == nil {
		log.Fatalf("FAIL: %s already existed when the call was abandoned — the whole chain ran "+
			"before the drop, so this run cannot show work continuing after it", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("FAIL: stat %s: %v", path, err)
	}

	// 4. Let it finish, with no call of any kind waiting on it.
	started := time.Now()
	deadline := started.Add(abandonDoneWindow)
	for {
		s := callTool(ctx, e.Session, "agent_task_status", map[string]any{"task_id": taskID})
		status := jsonField(s, "status")
		if status != "running" {
			fmt.Printf("  finished unattended %v after the abandon: status=%s error=%q\n",
				time.Since(started).Round(time.Second), status, jsonField(s, "error"))
			if status != "done" {
				log.Fatalf("FAIL: expected the abandoned turn to reach done, got %q (error=%q):\n%s",
					status, jsonField(s, "error"), indent(textContent(s)))
			}
			break
		}
		if time.Now().After(deadline) {
			log.Fatalf("FAIL: task %s was still running %s after the call was abandoned:\n%s",
				taskID, abandonDoneWindow, indent(textContent(s)))
		}
		if err := ctx.Err(); err != nil {
			log.Fatalf("FAIL: scenario context ended while waiting for task %s: %v", taskID, err)
		}
		time.Sleep(time.Second)
	}

	// 5. The point of the whole scenario. A status field can read "done" for a
	//    worker that was killed before it wrote anything; the file cannot. And
	//    because each link had to be read back before the next was written, the
	//    final content can only exist if every link ran — so this is evidence of
	//    a chain of work carried out unattended, not merely of a file appearing.
	body, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("FAIL: the abandoned worker reported done but never reached the end of the chain "+
			"— reading %s: %v. Status alone is not the proof; the work has to be on disk", path, err)
	}
	if strings.TrimSpace(string(body)) != want {
		log.Fatalf("FAIL: %s should carry %q, the only content a completed chain can produce; it holds:\n%s",
			path, want, indent(string(body)))
	}
	fmt.Printf("  side effect on disk: %s carries %q\n", path, want)
	fmt.Println("ABANDON DONE (the turn outlived the call that asked for it)")
}

// abandonLink names the file written by link k of the chain.
func abandonLink(marker string, k int) string {
	return fmt.Sprintf("%s-%d.txt", marker, k)
}

// abandonChain builds the delegated prompt and the content only a completed
// chain can produce.
//
// The obvious way to make a turn slow is to tell the agent to sleep, and it does
// not work — do not reach for it again. Claude Code refuses a foreground sleep
// outright ("Blocked: ... To wait for a condition, use Monitor with an
// until-loop ... Do not chain shorter sleeps to work around this block"), and
// asked to wait and then write, it backgrounds the wait and ends its turn on the
// spot — "I'll create the file once it completes" — so nothing is ever written.
// Both were confirmed against `claude -p` directly, with no MCP server involved.
//
// So the runtime comes from work instead. Each link has to read back the file
// the previous link wrote, which is two tool calls the agent cannot collapse
// into one and cannot start early. It also makes the final content load-bearing:
// "12345" is reachable only by walking every link, so finding it on disk says
// the worker did the whole chain after the call was dropped.
func abandonChain(marker string) (prompt, want string) {
	var p, w strings.Builder
	p.WriteString("Work through these steps strictly in order, one at a time. " +
		"Each step depends on the file the previous step wrote, so do not batch them, " +
		"do not run them in parallel, and do not skip ahead.\n\n")
	fmt.Fprintf(&p, "Step 1: create the file %s in the current directory, containing exactly: 1\n", abandonLink(marker, 1))
	w.WriteString("1")
	for k := 2; k <= abandonChainLinks; k++ {
		fmt.Fprintf(&p, "Step %d: read %s, then create %s containing that file's contents with %d "+
			"appended to the end of the line.\n", k, abandonLink(marker, k-1), abandonLink(marker, k), k)
		fmt.Fprintf(&w, "%d", k)
	}
	fmt.Fprintf(&p, "\nThen reply with the word DONE.\n\n"+
		"Every one of these files must hold a single line and nothing else. Actually read each "+
		"file before writing the next one — do not write any of them from memory or work out the "+
		"contents ahead of time. Do not touch any other file.")
	return p.String(), w.String()
}

// abandonFindTask waits for the run to be registered server-side and returns its
// id. The abandoned call never gets to hand back a task_id, so it has to be
// recovered from the listing; matching the marker rather than taking the newest
// entry keeps the proof pinned to this run whatever else the server holds.
func abandonFindTask(ctx context.Context, e *env, marker string) string {
	deadline := time.Now().Add(abandonStartWindow)
	for {
		res := callTool(ctx, e.Session, "agent_list_tasks", map[string]any{})
		for _, t := range abandonTasks(res) {
			for _, p := range t.Prompts {
				if strings.Contains(p, marker) {
					return t.ID
				}
			}
		}
		if time.Now().After(deadline) {
			log.Fatalf("FAIL: agent_run_task registered no task carrying %s within %s; last listing:\n%s",
				marker, abandonStartWindow, indent(textContent(res)))
		}
		if err := ctx.Err(); err != nil {
			log.Fatalf("FAIL: scenario context ended while waiting for the task to register: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// abandonAwaitOutput waits for the worker to stream something, so the call is
// dropped on a turn that is demonstrably doing work rather than one that has
// only been registered. It is a courtesy, not an assertion: a silent start
// still leaves everything below provable, so a run that streams nothing goes
// ahead rather than failing here.
func abandonAwaitOutput(ctx context.Context, e *env, before int) {
	deadline := time.Now().Add(abandonStartWindow)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if len(e.Progress()) > before {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Println("  (no progress streamed yet; dropping the call anyway)")
}

// abandonTasks decodes the task rows out of an agent_list_tasks result.
func abandonTasks(res *mcp.CallToolResult) []abandonTaskRow {
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return nil
	}
	var listing struct {
		Tasks []abandonTaskRow `json:"tasks"`
	}
	if err := json.Unmarshal(b, &listing); err != nil {
		return nil
	}
	return listing.Tasks
}
