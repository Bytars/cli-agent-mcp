// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	register("concurrency", scenario{
		// Needs is empty on purpose: the cap is enforced by the manager before a
		// worker is ever spawned, so it means the same thing against the mock and
		// against a real agent. The mock is the cheaper way to run it.
		Needs: "",
		What: "CLI_AGENT_MCP_MAX_CONCURRENT refuses the task that would exceed the cap, names the setting in the\n" +
			"refusal, and admits work again once a live worker goes away.\n" +
			"Launch the server with CLI_AGENT_MCP_MAX_CONCURRENT set to the same number as SMOKE_MAX_CONCURRENT\n" +
			"(default 3), otherwise this proves nothing.",
		Run: runConcurrency,
	})
}

// runConcurrency proves the live-worker cap, the only thing standing between a
// delegating model that fans out ten tasks and a machine running ten agents.
// MaxTasks bounds retained records, not processes, so every other limit still
// looks satisfied while the box falls over.
func runConcurrency(ctx context.Context, e *env) {
	limit := toInt(getenv("SMOKE_MAX_CONCURRENT", "3"))
	if limit <= 0 {
		log.Fatalf("FAIL: SMOKE_MAX_CONCURRENT=%q — this scenario needs a positive cap, and the server must have been "+
			"launched with CLI_AGENT_MCP_MAX_CONCURRENT set to the same value", os.Getenv("SMOKE_MAX_CONCURRENT"))
	}

	// The workers only have to outlive the assertions, so pick a prompt that
	// keeps one busy without doing anything to the working tree.
	prompt := "sleep:8000"
	if e.Agent != "mock" {
		prompt = "List every file tracked in this repository and describe each one in a single line. Do not modify anything."
	}

	var started []string
	defer func() {
		// Leave no workers behind for whatever runs next.
		for _, id := range started {
			_, _ = e.Session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "agent_cancel_task",
				Arguments: map[string]any{"task_id": id},
			})
		}
	}()

	fmt.Printf("  filling the cap: starting %d task(s)\n", limit)
	for i := 0; i < limit; i++ {
		res := concStart(ctx, e, prompt)
		if res.IsError {
			log.Fatalf("FAIL: task %d of %d was refused, but the cap is %d: %s",
				i+1, limit, limit, firstLine(textContent(res)))
		}
		id := jsonField(res, "task_id")
		if id == "" {
			log.Fatalf("FAIL: task %d of %d returned no task_id", i+1, limit)
		}
		started = append(started, id)
		fmt.Printf("    [%d] admitted %s\n", i+1, id)
	}

	// The refusal below is only meaningful once every one of those workers is
	// actually live; if one finished early the server is right to admit another.
	concWaitAllRunning(ctx, e, started, 20*time.Second)

	fmt.Println("  one more must be refused")
	over := concStart(ctx, e, prompt)
	if !over.IsError {
		if id := jsonField(over, "task_id"); id != "" {
			started = append(started, id) // still ours to clean up
		}
		log.Fatalf("FAIL: task %d was admitted with the cap at %d — the cap refuses nothing", limit+1, limit)
	}
	refusal := textContent(over)
	// An operator reading "limit reached" has nothing to act on. The refusal has
	// to name the knob, because naming it is the whole remedy.
	if !strings.Contains(refusal, "MAX_CONCURRENT") {
		log.Fatalf("FAIL: refusal does not name CLI_AGENT_MCP_MAX_CONCURRENT, so an operator cannot act on it: %s",
			firstLine(refusal))
	}
	fmt.Printf("    correctly refused: %s\n", firstLine(refusal))

	// Freeing a slot is the other half of the proof. A cap that never lets go is a
	// permanent ceiling, not a concurrency gate.
	victim := started[0]
	fmt.Printf("  cancelling %s to free a slot\n", victim)
	_, err := e.Session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "agent_cancel_task",
		Arguments: map[string]any{"task_id": victim},
	})
	if err != nil {
		log.Fatalf("agent_cancel_task: %v", err)
	}
	concWaitNotRunning(ctx, e, victim, 20*time.Second)

	// Cancel returns once the task stops reporting running, but admission reads
	// live workers through a separate path, so retry briefly rather than assert
	// on the first attempt and blame the cap for a scheduling lag.
	deadline := time.Now().Add(15 * time.Second)
	var replacement *mcp.CallToolResult
	for {
		replacement = concStart(ctx, e, prompt)
		if !replacement.IsError {
			break
		}
		if time.Now().After(deadline) {
			log.Fatalf("FAIL: still refused 15s after a worker was cancelled — the cap is a permanent ceiling, "+
				"not a live-worker gate: %s", firstLine(textContent(replacement)))
		}
		time.Sleep(250 * time.Millisecond)
	}
	id := jsonField(replacement, "task_id")
	if id == "" {
		log.Fatal("FAIL: the replacement task was admitted but returned no task_id")
	}
	started = append(started, id)
	fmt.Printf("    admitted again after the slot freed: %s\n", id)

	fmt.Printf("CONCURRENCY DONE (cap=%d held, refusal named the setting, slot reopened on cancel)\n", limit)
}

// concStart starts a background task and hands back the raw result, refusal and
// all. callTool cannot be used here: a refusal arrives as a tool error, which is
// exactly what this scenario is trying to read.
func concStart(ctx context.Context, e *env, prompt string) *mcp.CallToolResult {
	res, err := e.Session.CallTool(ctx, &mcp.CallToolParams{
		Name: "agent_start_task",
		Arguments: map[string]any{
			"prompt": prompt,
			"agent":  e.Agent,
			"cwd":    e.Cwd,
		},
	})
	if err != nil {
		log.Fatalf("agent_start_task: %v", err)
	}
	return res
}

func concWaitAllRunning(ctx context.Context, e *env, ids []string, within time.Duration) {
	deadline := time.Now().Add(within)
	for {
		var notYet []string
		for _, id := range ids {
			if concStatus(ctx, e, id) != "running" {
				notYet = append(notYet, id)
			}
		}
		if len(notYet) == 0 {
			return
		}
		if time.Now().After(deadline) {
			log.Fatalf("FAIL: timed out after %v waiting for every task to be live; still not running: %s. "+
				"Use a prompt that keeps a worker busy longer than the assertions take.",
				within, strings.Join(notYet, ", "))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func concWaitNotRunning(ctx context.Context, e *env, id string, within time.Duration) {
	deadline := time.Now().Add(within)
	for {
		status := concStatus(ctx, e, id)
		if status != "running" {
			fmt.Printf("    %s stopped: status=%s\n", id, status)
			return
		}
		if time.Now().After(deadline) {
			log.Fatalf("FAIL: %s was still running %v after cancel, so the slot never freed", id, within)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func concStatus(ctx context.Context, e *env, id string) string {
	res := callTool(ctx, e.Session, "agent_task_status", map[string]any{"task_id": id})
	return jsonField(res, "status")
}
