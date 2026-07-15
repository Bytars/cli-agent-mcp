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
