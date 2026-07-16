// Command smoketest drives the built cli-agent-mcp server over stdio using the
// MCP client, exercising the full delegate → poll → read pipeline against the
// built-in "mock" agent. It needs no Claude Code or Cursor install.
//
// Usage: go run ./cmd/smoketest [path-to-server-exe]
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	serverExe := "./cli-agent-mcp.exe"
	if len(os.Args) > 1 {
		serverExe = os.Args[1]
	}

	agentName := getenv("SMOKE_AGENT", "mock")
	prompt := getenv("SMOKE_PROMPT", "say hello from the worker")
	cwd := getenv("SMOKE_CWD", mustCwd())
	doFollowup := os.Getenv("SMOKE_FOLLOWUP") != "0"

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	cmd := exec.Command(serverExe)
	cmd.Env = append(os.Environ(),
		"CLI_AGENT_MCP_DEFAULT_AGENT="+agentName,
		// Pin the gate closed so the extra_args assertion is hermetic regardless
		// of what the developer happens to have exported.
		"CLI_AGENT_MCP_ALLOW_EXTRA_ARGS=false",
	)
	cmd.Stderr = os.Stderr

	var progressMu sync.Mutex
	var progressMsgs []string
	client := mcp.NewClient(&mcp.Implementation{Name: "smoketest", Version: "0.1.0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, r *mcp.ProgressNotificationClientRequest) {
			progressMu.Lock()
			progressMsgs = append(progressMsgs, r.Params.Message)
			progressMu.Unlock()
			fmt.Printf("  · progress[%v]: %s\n", r.Params.Progress, r.Params.Message)
		},
	})
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer session.Close()

	fmt.Println("== tools ==")
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			log.Fatalf("list tools: %v", err)
		}
		fmt.Printf("  - %s\n", t.Name)
	}

	// SMOKE_ONLY=plan isolates a single plan-only call, so a caller can prove the
	// agent executed nothing (e.g. by checking the filesystem afterward).
	if os.Getenv("SMOKE_ONLY") == "plan" {
		fmt.Println("\n== agent_plan_task (ONLY) ==")
		res := callTool(ctx, session, "agent_plan_task", map[string]any{
			"prompt": prompt,
			"agent":  agentName,
			"cwd":    cwd,
		})
		fmt.Printf("status=%s\n", jsonField(res, "status"))
		fmt.Printf("RESULT:\n%s\n", textContent(res))
		fmt.Println("PLAN-ONLY DONE")
		return
	}

	// SMOKE_ONLY=watchstream proves a SINGLE until=done watch call streams live
	// progress while it blocks, and returns only when the task is done.
	if os.Getenv("SMOKE_ONLY") == "watchstream" {
		fmt.Println("\n== agent_watch until=done streams live in ONE call ==")
		st := callTool(ctx, session, "agent_start_task", map[string]any{
			"prompt": "sleep:4500", "agent": agentName, "cwd": cwd,
		})
		id := jsonField(st, "task_id")
		before := progressLen(&progressMu, &progressMsgs)
		wp := &mcp.CallToolParams{Name: "agent_watch", Arguments: map[string]any{"task_id": id}}
		wp.SetProgressToken("watch-1")
		start := time.Now()
		res, err := session.CallTool(ctx, wp)
		if err != nil {
			log.Fatalf("agent_watch: %v", err)
		}
		elapsed := time.Since(start)
		streamed := progressLen(&progressMu, &progressMsgs) - before
		fmt.Printf("  single call blocked %v, status=%s, streamed %d progress updates\n",
			elapsed.Round(time.Millisecond), jsonField(res, "status"), streamed)
		if jsonField(res, "running") != "false" {
			log.Fatal("FAIL: until=done returned while still running")
		}
		if elapsed < 3*time.Second {
			log.Fatal("FAIL: until=done returned too early (didn't actually wait for completion)")
		}
		if streamed < 2 {
			log.Fatalf("FAIL: expected live progress during the wait, got %d", streamed)
		}
		fmt.Println("WATCHSTREAM-ONLY DONE")
		return
	}

	// SMOKE_ONLY=timeout proves a hung task is killed by the server timeout
	// (run the server with CLI_AGENT_MCP_TASK_TIMEOUT_SECONDS set).
	if os.Getenv("SMOKE_ONLY") == "timeout" {
		fmt.Println("\n== task timeout (anti-deadlock) ==")
		st := callTool(ctx, session, "agent_start_task", map[string]any{
			"prompt": "sleep:15000",
			"agent":  agentName,
			"cwd":    cwd,
		})
		id := jsonField(st, "task_id")
		var status string
		for i := 0; i < 60; i++ {
			s := callTool(ctx, session, "agent_task_status", map[string]any{"task_id": id})
			status = jsonField(s, "status")
			if status != "running" {
				fmt.Printf("  final status=%s\n  error=%s\n", status, jsonField(s, "error"))
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if status != "failed" {
			log.Fatalf("FAIL: expected timed-out task to be failed, got %q", status)
		}
		fmt.Println("TIMEOUT-ONLY DONE")
		return
	}

	// SMOKE_ONLY=cancel starts a long-running task and interrupts it, proving the
	// worker actually stops before it would have finished.
	if os.Getenv("SMOKE_ONLY") == "cancel" {
		fmt.Println("\n== agent_cancel_task (interrupt a running task) ==")
		st := callTool(ctx, session, "agent_start_task", map[string]any{
			"prompt": "sleep:10000", // mock would run ~10s if left alone
			"agent":  agentName,
			"cwd":    cwd,
		})
		id := jsonField(st, "task_id")
		fmt.Printf("  started %s (would run ~10s)\n", id)
		time.Sleep(700 * time.Millisecond) // let it get going
		start := time.Now()
		cr := callTool(ctx, session, "agent_cancel_task", map[string]any{"task_id": id})
		// Give the run goroutine a beat to finalize, then read status.
		time.Sleep(400 * time.Millisecond)
		final := callTool(ctx, session, "agent_task_status", map[string]any{"task_id": id})
		elapsed := time.Since(start)
		fmt.Printf("  cancel returned status=%s; final status=%s after %v\n",
			jsonField(cr, "status"), jsonField(final, "status"), elapsed.Round(time.Millisecond))
		if jsonField(final, "status") != "canceled" {
			log.Fatalf("FAIL: expected status canceled, got %q", jsonField(final, "status"))
		}
		if elapsed > 5*time.Second {
			log.Fatalf("FAIL: cancel took too long (%v) — the process was not really interrupted", elapsed)
		}
		fmt.Println("CANCEL-ONLY DONE (interrupt worked)")
		return
	}

	// 0. streaming run with live progress notifications
	fmt.Println("\n== agent_run_task (streaming, no polling) ==")
	runParams := &mcp.CallToolParams{Name: "agent_run_task", Arguments: map[string]any{
		"prompt": prompt,
		"agent":  agentName,
		"cwd":    cwd,
	}}
	runParams.SetProgressToken("run-1")
	runRes, err := session.CallTool(ctx, runParams)
	if err != nil {
		log.Fatalf("agent_run_task: %v", err)
	}
	runStatus := jsonField(runRes, "status")
	progressMu.Lock()
	nProgress := len(progressMsgs)
	progressMu.Unlock()
	fmt.Printf("  run_task returned status=%s (received %d progress notifications)\n", runStatus, nProgress)
	if nProgress < 1 {
		log.Fatal("FAIL: expected at least one progress notification during agent_run_task")
	}
	if agentName == "mock" {
		progressMu.Lock()
		sawToolLine := false
		for _, m := range progressMsgs {
			if strings.HasPrefix(m, "↳") {
				sawToolLine = true
			}
		}
		progressMu.Unlock()
		if !sawToolLine {
			log.Fatal("FAIL: expected a progress notification with the tool's last output line (↳ ...)")
		}
	}

	// 0b. extra_args must be refused unless the operator opted in.
	fmt.Println("\n== extra_args gate (must be refused by default) ==")
	gateRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "agent_run_task", Arguments: map[string]any{
		"prompt":     prompt,
		"agent":      agentName,
		"cwd":        cwd,
		"extra_args": []string{"--dangerously-skip-permissions"},
	}})
	if err != nil {
		log.Fatalf("agent_run_task (extra_args): %v", err)
	}
	if !gateRes.IsError {
		log.Fatal("FAIL: extra_args was accepted; it must be refused unless CLI_AGENT_MCP_ALLOW_EXTRA_ARGS=true")
	}
	fmt.Printf("  correctly refused: %s\n", firstLine(textContent(gateRes)))

	// 0c. plan-only run must not execute.
	fmt.Println("\n== agent_plan_task (plan only, nothing executed) ==")
	planRes := callTool(ctx, session, "agent_plan_task", map[string]any{
		"prompt": prompt,
		"agent":  agentName,
		"cwd":    cwd,
	})
	planStatus := jsonField(planRes, "status")
	planResult := jsonField(planRes, "result")
	fmt.Printf("  status=%s\n  result=%s\n", planStatus, firstLine(planResult))
	if agentName == "mock" {
		if planStatus != "done" {
			log.Fatalf("FAIL: expected plan status done, got %q", planStatus)
		}
		if !strings.Contains(planResult, "PLAN") {
			log.Fatalf("FAIL: expected a plan in the result, got %q", planResult)
		}
	}

	// 0d. background mode: start, then a SINGLE agent_watch that waits to completion.
	fmt.Println("\n== agent_watch until=done (single call, waits to completion) ==")
	ws := callTool(ctx, session, "agent_start_task", map[string]any{
		"prompt": prompt,
		"agent":  agentName,
		"cwd":    cwd,
	})
	wtask := jsonField(ws, "task_id")
	wr := callTool(ctx, session, "agent_watch", map[string]any{"task_id": wtask}) // until defaults to "done"
	running := jsonField(wr, "running")
	wstatus := jsonField(wr, "status")
	fmt.Printf("  one call → running=%s status=%s\n  result=%s\n", running, wstatus, firstLine(jsonField(wr, "text")))
	if running != "false" {
		log.Fatal("FAIL: a single until=done agent_watch must return only when the task has finished")
	}
	if wstatus != "done" && agentName == "mock" {
		log.Fatalf("FAIL: expected status done, got %q", wstatus)
	}

	// until=change should return promptly with a chunk (interruptible supervision).
	fmt.Println("\n== agent_watch until=change (peek) ==")
	ws2 := callTool(ctx, session, "agent_start_task", map[string]any{"prompt": prompt, "agent": agentName, "cwd": cwd})
	cr := callTool(ctx, session, "agent_watch", map[string]any{"task_id": jsonField(ws2, "task_id"), "until": "change"})
	fmt.Printf("  change → running=%s next_since_line=%s\n", jsonField(cr, "running"), jsonField(cr, "next_since_line"))

	// 1. start a task
	fmt.Println("\n== agent_start_task ==")
	start := callTool(ctx, session, "agent_start_task", map[string]any{
		"prompt": prompt,
		"agent":  agentName,
		"cwd":    cwd,
	})
	taskID := jsonField(start, "task_id")
	fmt.Printf("  task_id=%s status=%s\n", taskID, jsonField(start, "status"))
	if taskID == "" {
		log.Fatal("no task_id returned")
	}

	// 2. poll status
	fmt.Println("\n== agent_task_status (poll) ==")
	var status string
	for i := 0; i < 170; i++ {
		st := callTool(ctx, session, "agent_task_status", map[string]any{"task_id": taskID})
		status = jsonField(st, "status")
		fmt.Printf("  [%d] status=%s session_id=%s result=%q\n", i, status, jsonField(st, "session_id"), jsonField(st, "result"))
		if status == "done" || status == "failed" || status == "canceled" {
			break
		}
		time.Sleep(time.Second)
	}

	// 3. read output
	fmt.Println("\n== agent_get_output ==")
	out := callTool(ctx, session, "agent_get_output", map[string]any{"task_id": taskID})
	fmt.Printf("  total_lines=%s\n  text:\n%s\n", jsonField(out, "total_lines"), indent(textContent(out)))

	// 4. follow-up on the same session (optional)
	fuStatus, turns := "done", "2"
	if doFollowup {
		fmt.Println("\n== agent_send_followup ==")
		fu := callTool(ctx, session, "agent_send_followup", map[string]any{
			"task_id": taskID,
			"prompt":  "now say goodbye",
		})
		fmt.Printf("  resumed status=%s session_id=%s\n", jsonField(fu, "status"), jsonField(fu, "session_id"))
		fuStatus, turns = "", ""
		for i := 0; i < 170; i++ {
			st := callTool(ctx, session, "agent_task_status", map[string]any{"task_id": taskID})
			fuStatus = jsonField(st, "status")
			turns = jsonField(st, "turns")
			if fuStatus == "done" || fuStatus == "failed" {
				fmt.Printf("  final status=%s turns=%s\n", fuStatus, turns)
				break
			}
			time.Sleep(time.Second)
		}
	} else {
		fmt.Println("\n== agent_send_followup == (skipped)")
	}

	// 5. list tasks
	fmt.Println("\n== agent_list_tasks ==")
	lt := callTool(ctx, session, "agent_list_tasks", map[string]any{})
	fmt.Printf("  %s\n", textContent(lt))

	if status != "done" {
		log.Fatalf("FAIL: expected first-turn status done, got %q", status)
	}
	if doFollowup {
		if fuStatus != "done" {
			log.Fatalf("FAIL: expected follow-up status done, got %q", fuStatus)
		}
		if turns != "2" {
			log.Fatalf("FAIL: expected 2 turns after follow-up, got %q", turns)
		}
	}
	fmt.Println("\nSMOKETEST PASSED")
}

func callTool(ctx context.Context, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		log.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		log.Fatalf("call %s returned tool error: %s", name, textContent(res))
	}
	return res
}

func textContent(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// jsonField pulls a top-level field from the tool's StructuredContent.
func jsonField(res *mcp.CallToolResult, key string) string {
	if res.StructuredContent == nil {
		return ""
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	if v, ok := m[key]; ok {
		switch vv := v.(type) {
		case string:
			return vv
		default:
			return fmt.Sprintf("%v", vv)
		}
	}
	return ""
}

func progressLen(mu *sync.Mutex, msgs *[]string) int {
	mu.Lock()
	defer mu.Unlock()
	return len(*msgs)
}

func toInt(s string) int {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return int(f)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	return wd
}

func indent(s string) string {
	if s == "" {
		return "    (empty)"
	}
	out := ""
	for _, line := range splitLines(s) {
		out += "    " + line + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}
