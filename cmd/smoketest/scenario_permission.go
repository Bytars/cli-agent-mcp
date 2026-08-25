// SPDX-License-Identifier: Apache-2.0

package main

// The "permission" scenario. It needs a real agent — the mock never asks for
// anything — and it needs the operator to have lowered the watch window:
//
//	CLI_AGENT_MCP_WATCH_WINDOW_SECONDS=10 \
//	SMOKE_ONLY=permission SMOKE_AGENT=claude SMOKE_CWD=/path/to/scratch/repo \
//	go run ./cmd/smoketest ./cli-agent-mcp.exe
//
// Without that, step 1 sits on the default 50s window before it reports back,
// and the whole run has only the 180s main allows it.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	register("permission", scenario{
		Needs: "real",
		What: "A worker that needs a tool it was not pre-approved for parks its request instead of stalling: " +
			"agent_list_permissions shows it with the command at stake, agent_answer_permission releases it, " +
			"and the work it was blocked on actually lands on disk.",
		Run: runPermissionScenario,
	})
}

// How long each wait is allowed to take. Both are bounded because the failure
// this scenario is most likely to catch is a worker that never resumes, and a
// hang reports nothing.
const (
	permRequestWait = 60 * time.Second
	permFinishWait  = 75 * time.Second
)

// permWaiting mirrors the entries agent_list_permissions returns under
// "waiting" (internal/task.PermissionRequest).
type permWaiting struct {
	ID      string `json:"id"`
	TaskID  string `json:"task_id"`
	Tool    string `json:"tool"`
	Detail  string `json:"detail"`
	Command string `json:"command"`
}

func runPermissionScenario(ctx context.Context, e *env) {
	// A name nothing else could have created, so step 5 cannot pass on a
	// leftover directory from an earlier run.
	dirName := fmt.Sprintf("smoketest-permission-%d", time.Now().UnixNano())
	target := filepath.Join(e.Cwd, dirName)

	// Creating a directory is the cheapest thing that changes state, and state
	// is what makes the agent ask: a read-only command would be approved
	// without anyone being consulted and nothing would ever park.
	prompt := fmt.Sprintf(
		"Run one shell command that creates a directory named %s in the current working directory, "+
			"then stop and say DONE. Use a shell command such as mkdir — do not use a file-writing tool, "+
			"and do not create, change or delete anything else.", dirName)

	fmt.Printf("1. delegating a task that cannot proceed without an answer\n   cwd=%s\n   expects=%s\n", e.Cwd, target)
	run := callTool(ctx, e.Session, "agent_run_task", map[string]any{
		"prompt": prompt,
		"agent":  e.Agent,
		"cwd":    e.Cwd,
	})
	taskID := jsonField(run, "task_id")
	if taskID == "" {
		log.Fatalf("FAIL: agent_run_task returned no task_id:\n%s", indent(textContent(run)))
	}
	// "STILL RUNNING" is the expected outcome here, not a failure: the worker is
	// parked on its request and the call returned when its window closed. Only a
	// worker that died before it could ask invalidates everything below.
	runStatus := jsonField(run, "status")
	if runStatus == "failed" || runStatus == "canceled" {
		log.Fatalf("FAIL: the worker died before it could ask for anything (status %q):\n%s",
			runStatus, indent(textContent(run)))
	}
	fmt.Printf("   task_id=%s status=%s\n   %s\n", taskID, runStatus, firstLine(textContent(run)))

	fmt.Printf("2. polling agent_list_permissions for a parked request (up to %s)\n", permRequestWait)
	var req permWaiting
	deadline := time.Now().Add(permRequestWait)
	for {
		if w, ok := permWaitingFor(ctx, e, taskID); ok {
			req = w
			break
		}
		if time.Now().After(deadline) {
			log.Fatalf("FAIL: task %s never appeared under \"waiting\" within %s. "+
				"Either the worker was never asked (the tool is pre-approved, or CLI_AGENT_MCP_ASK_PERMISSION=false) "+
				"or the request was answered by something other than this scenario.\nlast task status:\n%s",
				taskID, permRequestWait, indent(textContent(
					callTool(ctx, e.Session, "agent_task_status", map[string]any{"task_id": taskID}))))
		}
		time.Sleep(time.Second)
	}
	// The tool name alone ("may the agent run Bash") is not a question anyone
	// can answer, so the detail carrying the actual command is load-bearing.
	// Its exact text is not asserted — the agent picks its own spelling of
	// mkdir — because step 5 is what proves the right command ran.
	if req.Tool == "" {
		log.Fatalf("FAIL: parked request %s names no tool: %+v", req.ID, req)
	}
	if strings.TrimSpace(req.Detail) == "" {
		log.Fatalf("FAIL: parked request %s (%s) carries no detail, so nobody could answer it: %+v",
			req.ID, req.Tool, req)
	}
	fmt.Printf("   %s parked: %s — %s\n", req.ID, req.Tool, firstLine(req.Detail))

	fmt.Println("3. answering it with allow=true")
	ans := callTool(ctx, e.Session, "agent_answer_permission", map[string]any{
		"task_id":    taskID,
		"request_id": req.ID,
		"allow":      true,
	})
	// remember is deliberately left off: a scenario that writes a permanent
	// grant would pre-approve itself and pass without parking anything the next
	// time it runs.
	if st := jsonField(ans, "status"); st != "running" && st != "done" {
		log.Fatalf("FAIL: after being allowed, the task should be running (or already finished), got %q:\n%s",
			st, indent(textContent(ans)))
	}
	fmt.Printf("   %s\n", firstLine(textContent(ans)))

	fmt.Printf("4. waiting for the released worker to finish (up to %s)\n", permFinishWait)
	var status string
	var last *mcp.CallToolResult
	deadline = time.Now().Add(permFinishWait)
	for {
		last = callTool(ctx, e.Session, "agent_task_status", map[string]any{"task_id": taskID})
		status = jsonField(last, "status")
		if status != "running" {
			break
		}
		// An agent is free to ask more than once — a second mkdir attempt, a
		// verifying command. Those are not what this scenario is about, but
		// leaving one parked would stall it into the deadline and report the
		// resume as broken when it was not.
		if w, ok := permWaitingFor(ctx, e, taskID); ok {
			fmt.Printf("   also allowing %s: %s\n", w.Tool, firstLine(w.Detail))
			callTool(ctx, e.Session, "agent_answer_permission", map[string]any{
				"task_id": taskID, "request_id": w.ID, "allow": true,
			})
		}
		if time.Now().After(deadline) {
			log.Fatalf("FAIL: task %s was allowed but never finished within %s — the answer did not release the worker.\n"+
				"last status:\n%stranscript:\n%s",
				taskID, permFinishWait, indent(textContent(last)), indent(permTranscript(ctx, e, taskID)))
		}
		time.Sleep(time.Second)
	}
	if status != "done" {
		log.Fatalf("FAIL: expected the released worker to reach status done, got %q:\n%stranscript:\n%s",
			status, indent(textContent(last)), indent(permTranscript(ctx, e, taskID)))
	}
	fmt.Printf("   final status=%s\n", status)

	// The transcript is where a parked request is announced and where its
	// verdict is recorded, so it is the evidence that the worker went through
	// this route rather than giving up and carrying on without the tool.
	transcript := permTranscript(ctx, e, taskID)
	if !strings.Contains(transcript, req.ID) {
		log.Fatalf("FAIL: the transcript never mentions request %s:\n%s", req.ID, indent(transcript))
	}
	if !strings.Contains(transcript, req.ID+" ALLOWED") {
		log.Fatalf("FAIL: the transcript has no ALLOWED verdict for %s, so the worker was not released by the answer:\n%s",
			req.ID, indent(transcript))
	}

	fmt.Println("5. checking the side effect on disk")
	info, err := os.Stat(target)
	if err != nil {
		log.Fatalf("FAIL: the task finished done but %s does not exist (%v) — the bookkeeping moved, the work did not.\ntranscript:\n%s",
			target, err, indent(transcript))
	}
	if !info.IsDir() {
		log.Fatalf("FAIL: %s exists but is not a directory", target)
	}
	// Ours by construction and empty if the agent did as it was told, so
	// removing it keeps a scratch repo from filling up with one per run. A
	// non-empty one is left alone by os.Remove, which is the right outcome:
	// nothing this scenario created is worth deleting recursively.
	_ = os.Remove(target)

	fmt.Printf("   %s existed — the worker parked, was answered, and did the work\n", target)
}

// permWaitingFor returns the request parked against this task, if there is one.
func permWaitingFor(ctx context.Context, e *env, taskID string) (permWaiting, bool) {
	res := callTool(ctx, e.Session, "agent_list_permissions", map[string]any{})
	if res.StructuredContent == nil {
		log.Fatalf("FAIL: agent_list_permissions returned no structured content:\n%s", indent(textContent(res)))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		log.Fatalf("FAIL: agent_list_permissions result could not be re-encoded: %v", err)
	}
	var body struct {
		Waiting []permWaiting `json:"waiting"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		log.Fatalf("FAIL: agent_list_permissions result did not parse: %v\n%s", err, indent(string(b)))
	}
	for _, w := range body.Waiting {
		if w.TaskID == taskID {
			return w, true
		}
	}
	return permWaiting{}, false
}

// permTranscript reads the raw transcript, which is where the [permission]
// lines live; the compact view drops the request ids.
func permTranscript(ctx context.Context, e *env, taskID string) string {
	out := callTool(ctx, e.Session, "agent_get_output", map[string]any{"task_id": taskID, "raw": true})
	return textContent(out)
}
