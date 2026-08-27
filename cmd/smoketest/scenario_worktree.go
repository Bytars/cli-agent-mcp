// SPDX-License-Identifier: Apache-2.0

package main

// The "worktree" scenario. It needs a real agent, because the whole proof rests
// on a worker actually editing a file — the mock never touches disk, so against
// it every assertion below would pass without isolation existing at all.
//
//	CLI_AGENT_MCP_WATCH_WINDOW_SECONDS=10 \
//	SMOKE_ONLY=worktree SMOKE_AGENT=claude SMOKE_CWD=/path/to/scratch/repo \
//	go run ./cmd/smoketest ./cli-agent-mcp.exe
//
// The short watch window is not required — every wait here polls a status — but
// without it agent_run_task sits on the default 50s before it reports back, and
// the whole run has only the 180s main allows it.
//
// What is being proved is a trade, and both halves of it are asserted. An
// isolated task does not disturb the directory the caller asked about, which is
// what lets two workers run at once without producing a diff neither intended;
// and the price is that the work is then somewhere else, reachable only through
// the worktree path the reply names and agent_task_diff.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/winspawn"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	register("worktree", scenario{
		Needs: "real",
		What: "a task asked to isolate itself runs in a git worktree of its own and leaves the caller's\n" +
			"directory exactly as it found it. The reply says where the work went, agent_task_diff\n" +
			"shows it there, and agent_remove_worktree refuses to throw away uncommitted work\n" +
			"that exists nowhere else until it is told to force it.",
		Run: runWorktree,
	})
}

// How long the isolated edit is allowed to take. One append to one file is a
// short turn against a real agent; the budget is generous because the failure
// worth reporting is "it never finished", not "it was slow".
const worktreeFinishWait = 120 * time.Second

// The file the agent is told to change. It has to be tracked, because the
// central assertion is that a tracked file in the caller's directory is
// untouched, and an untracked one would be missing from the worktree entirely.
const worktreeFile = "README.md"

// worktreeReport mirrors the part of gitx.Report that agent_task_diff returns as
// structured content. Only the fields this scenario reads are here; jsonField
// cannot reach into the file list.
type worktreeReport struct {
	Root   string `json:"root"`
	Branch string `json:"branch"`
	Files  []struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	} `json:"files"`
	Patch string `json:"patch"`
	Dirty bool   `json:"dirty"`
}

func runWorktree(ctx context.Context, e *env) {
	// Declared up front so the cleanup closure below can see them: it has to be
	// able to run from the very first assertion that can fail, and by then only
	// some of these are known.
	var taskID, worktree, repo, branch string

	cleaned := false
	// A leaked worktree is not a cosmetic problem: it stays checked out in the
	// scratch repository, on a branch of its own, and every later run has to
	// step around it. log.Fatalf skips deferred functions, so failures go
	// through failf, which tidies up first.
	cleanup := func() {
		if cleaned {
			return
		}
		cleaned = true
		// A context of its own: this runs on the way out of a failure, and the
		// scenario's own context may be the thing that ended.
		cctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if taskID != "" {
			_, _ = e.Session.CallTool(cctx, &mcp.CallToolParams{
				Name:      "agent_remove_worktree",
				Arguments: map[string]any{"task_id": taskID, "force": true},
			})
		}
		// And again by hand, because the tool call above is exactly what may
		// have been broken when the assertion failed.
		if repo != "" {
			if worktree != "" {
				if _, err := os.Stat(worktree); err == nil {
					_, _ = gitOut(repo, "worktree", "remove", "--force", worktree)
				}
				_, _ = gitOut(repo, "worktree", "prune")
			}
			if branch != "" {
				_, _ = gitOut(repo, "branch", "-D", branch)
			}
		}
	}
	defer cleanup()

	failf := func(format string, args ...any) {
		cleanup()
		log.Fatalf(format, args...)
	}

	// 1. What the caller's directory looks like before anyone touches it. This
	//    is the baseline the central assertion compares against, so it is taken
	//    before the task exists rather than reconstructed afterwards.
	if out, err := gitOut(e.Cwd, "ls-files", "--error-unmatch", worktreeFile); err != nil {
		failf("FAIL: %s does not track %s, so \"the original was left alone\" would prove nothing.\n"+
			"Point SMOKE_CWD at a scratch git repository with a committed %s (scripts/e2e.ps1 makes one).\ngit said: %v\n%s",
			e.Cwd, worktreeFile, worktreeFile, err, indent(out))
	}
	originalPath := filepath.Join(e.Cwd, worktreeFile)
	beforeBody, err := os.ReadFile(originalPath)
	if err != nil {
		failf("FAIL: reading %s before the task: %v", originalPath, err)
	}
	beforeStatus := mustGitOut(failf, e.Cwd, "status", "--porcelain")
	fmt.Printf("0. baseline in %s\n   %s is %d bytes, git status --porcelain has %d line(s)\n",
		e.Cwd, worktreeFile, len(beforeBody), len(statusLines(beforeStatus)))

	// A marker nothing else could have written, so finding it proves this run
	// put it there and no leftover from an earlier one can stand in for it.
	marker := fmt.Sprintf("SMOKETEST-WORKTREE-%d", time.Now().UnixNano())

	// Exactly one edit, spelled out. A vaguer instruction is satisfiable in ways
	// the assertions below would miss — a new file, a reworded line — and then
	// the scenario reports a bug in isolation that is really a bug in its own
	// prompt. Note also there is no waiting of any kind here: Claude Code blocks
	// a foreground sleep outright, and asked to wait and then act it backgrounds
	// the wait and ends its turn (see abandonChain in scenario_abandon.go).
	prompt := fmt.Sprintf(
		"Append exactly one new line to the end of the file %s in the current working directory. "+
			"That line must be exactly:\n\n%s\n\n"+
			"Change nothing else in that file, and do not create, edit or delete any other file. "+
			"Do not run any git command — do not stage, commit, stash or check out anything. "+
			"Then reply with the word DONE.", worktreeFile, marker)

	fmt.Printf("1. delegating with isolate=true\n   cwd=%s\n   marker=%s\n", e.Cwd, marker)
	run := callTool(ctx, e.Session, "agent_run_task", map[string]any{
		"prompt":  prompt,
		"agent":   e.Agent,
		"cwd":     e.Cwd,
		"isolate": true,
		// Pre-approved because the permission mode is not what is under test
		// here: an unanswered request would park the worker and this would fail
		// as a timeout with nothing to say about isolation.
		"allowed_tools": []string{"Read", "Edit", "Write", "Bash", "PowerShell"},
	})
	taskID = jsonField(run, "task_id")
	worktree = jsonField(run, "worktree")
	repo = jsonField(run, "repo")
	branch = jsonField(run, "branch")
	if taskID == "" {
		failf("FAIL: agent_run_task returned no task_id:\n%s", indent(textContent(run)))
	}
	if st := jsonField(run, "status"); st == "failed" || st == "canceled" {
		failf("FAIL: the isolated worker died (status %q) before it could change anything:\n%s",
			st, indent(textContent(run)))
	}

	// 2. The structured result has to say where the work is going. Without these
	//    three fields an isolated run is indistinguishable from an ordinary one
	//    that quietly changed nothing.
	if worktree == "" || repo == "" || branch == "" {
		failf("FAIL: isolate=true but the result names no worktree (worktree=%q repo=%q branch=%q) — "+
			"the task ran in the caller's directory after all:\n%s",
			worktree, repo, branch, indent(textContent(run)))
	}
	if samePath(worktree, e.Cwd) {
		failf("FAIL: the reported worktree %s IS the caller's directory %s, so nothing was isolated",
			worktree, e.Cwd)
	}
	fmt.Printf("   task_id=%s\n   worktree=%s\n   branch=%s (cut from %s)\n", taskID, worktree, branch, repo)

	// 3. And the prose has to say it too. A caller who reads only the reply must
	//    still learn that their directory is not where the work landed; asserting
	//    on a fragment rather than the whole sentence leaves workspaceNote free
	//    to be reworded.
	note := textContent(run)
	for _, want := range []string{"ran isolated in", worktree, branch} {
		if !strings.Contains(note, want) {
			failf("FAIL: the reply never mentions %q, so a caller reading it would look for the "+
				"changes in the wrong directory:\n%s", want, indent(note))
		}
	}
	fmt.Println("   the reply carries the isolation note")

	// 4. Let the edit actually happen. agent_run_task returns when its watch
	//    window closes, which is usually before the worker is finished.
	status := worktreeAwaitDone(ctx, e, taskID, failf)
	fmt.Printf("2. worker finished: status=%s\n", status)

	// 5. The central assertion. Everything above could be bookkeeping; this is
	//    the behaviour. If isolation is broken the agent edited the caller's
	//    file, and this is what catches it.
	afterBody, err := os.ReadFile(originalPath)
	if err != nil {
		failf("FAIL: reading %s after the task: %v", originalPath, err)
	}
	if !bytes.Equal(beforeBody, afterBody) {
		failf("FAIL: the isolated task changed %s in the caller's directory. That is the one thing "+
			"isolation exists to prevent — two workers in one checkout overwrite each other.\nbefore:\n%safter:\n%s",
			originalPath, indent(string(beforeBody)), indent(string(afterBody)))
	}
	afterStatus := mustGitOut(failf, e.Cwd, "status", "--porcelain")
	if afterStatus != beforeStatus {
		failf("FAIL: git status in %s changed while an isolated task ran, so the worker touched it:\nbefore:\n%safter:\n%s",
			e.Cwd, indent(beforeStatus), indent(afterStatus))
	}
	fmt.Printf("3. %s is untouched: %s is byte-identical and git status is unchanged\n", e.Cwd, worktreeFile)

	// 6. The other half of the trade: the edit is real, it is just somewhere
	//    else. Read through the worktree path the reply gave, since that path is
	//    the only way a caller has of finding it.
	isolatedPath := filepath.Join(worktree, worktreeFile)
	isolatedBody, err := os.ReadFile(isolatedPath)
	if err != nil {
		failf("FAIL: reading %s: %v — the caller's copy was left alone, but nothing was written in "+
			"the worktree either, so the task did no work at all", isolatedPath, err)
	}
	if !strings.Contains(string(isolatedBody), marker) {
		failf("FAIL: %s does not carry %s. The worker ran isolated but never made the edit it was "+
			"asked for, so the untouched original above proves nothing:\n%s",
			isolatedPath, marker, indent(string(isolatedBody)))
	}
	fmt.Printf("4. the edit landed in the worktree: %s carries the marker\n", isolatedPath)

	// 7. agent_task_diff is how a caller reviews an isolated run without knowing
	//    any of this, so it has to report the change against the worktree rather
	//    than against the directory that was asked about.
	rep := worktreeDiff(ctx, e, taskID, false, failf)
	if !worktreeLists(rep, worktreeFile) {
		failf("FAIL: agent_task_diff does not report %s as changed, so an isolated run cannot be "+
			"reviewed: %+v", worktreeFile, rep.Files)
	}
	fmt.Printf("5. agent_task_diff reports %d changed file(s) on %s, including %s\n",
		len(rep.Files), rep.Branch, worktreeFile)

	// The refusal in step 8 only happens while the work is uncommitted, which is
	// what the prompt asked for. Saying so here turns an otherwise baffling
	// failure below into a one-line diagnosis.
	if !rep.Dirty {
		failf("FAIL: the worktree reports no uncommitted changes, so the agent committed its work "+
			"despite being told not to run git. agent_remove_worktree has nothing to refuse and the "+
			"rest of this scenario cannot be proved: %+v", rep)
	}

	patched := worktreeDiff(ctx, e, taskID, true, failf)
	if !strings.Contains(patched.Patch, marker) {
		failf("FAIL: the patch from agent_task_diff does not contain %s, so it is not showing the "+
			"work that was actually done:\n%s", marker, indent(patched.Patch))
	}
	fmt.Println("6. patch=true returns the diff itself, marker and all")

	// 8. Removing the worktree throws away the only copy of that work. Refusing
	//    is the point; callTool cannot be used because it fatals on a tool error,
	//    and here the error IS the assertion.
	refuse, err := e.Session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "agent_remove_worktree",
		Arguments: map[string]any{"task_id": taskID},
	})
	if err != nil {
		failf("FAIL: agent_remove_worktree: %v", err)
	}
	if !refuse.IsError {
		failf("FAIL: agent_remove_worktree deleted a worktree holding uncommitted work without being "+
			"forced. That work existed nowhere else and is now gone:\n%s", indent(textContent(refuse)))
	}
	if !strings.Contains(strings.ToLower(textContent(refuse)), "force") {
		failf("FAIL: agent_remove_worktree refused, but without telling the caller how to proceed "+
			"— the refusal must name force:\n%s", indent(textContent(refuse)))
	}
	fmt.Printf("7. correctly refused: %s\n", firstLine(textContent(refuse)))
	if _, err := os.Stat(worktree); err != nil {
		failf("FAIL: agent_remove_worktree reported a refusal but %s is gone anyway: %v", worktree, err)
	}

	// 9. And with force it goes, for real, off disk.
	rm := callTool(ctx, e.Session, "agent_remove_worktree", map[string]any{
		"task_id": taskID,
		"force":   true,
	})
	if _, err := os.Stat(worktree); !errors.Is(err, os.ErrNotExist) {
		failf("FAIL: agent_remove_worktree(force) reported success but %s is still on disk (%v):\n%s",
			worktree, err, indent(textContent(rm)))
	}
	cleaned = true
	fmt.Printf("8. forced removal took %s off disk\n", worktree)

	fmt.Println("WORKTREE DONE (the work happened, and it happened somewhere else)")
}

// worktreeAwaitDone polls until the isolated task stops running, bounded, and
// insists it got there by finishing rather than by failing.
func worktreeAwaitDone(ctx context.Context, e *env, taskID string, failf func(string, ...any)) string {
	deadline := time.Now().Add(worktreeFinishWait)
	for {
		st := callTool(ctx, e.Session, "agent_task_status", map[string]any{"task_id": taskID})
		status := jsonField(st, "status")
		if status != "running" {
			if status != "done" {
				failf("FAIL: expected the isolated task to reach done, got %q (error=%q):\n%s",
					status, jsonField(st, "error"), indent(textContent(st)))
			}
			return status
		}
		if time.Now().After(deadline) {
			failf("FAIL: task %s was still running %s after it was delegated; it never made the edit:\n%s",
				taskID, worktreeFinishWait, indent(textContent(st)))
		}
		if err := ctx.Err(); err != nil {
			failf("FAIL: scenario context ended while waiting for task %s: %v", taskID, err)
		}
		time.Sleep(time.Second)
	}
}

// worktreeDiff calls agent_task_diff and decodes its structured content.
func worktreeDiff(ctx context.Context, e *env, taskID string, patch bool, failf func(string, ...any)) worktreeReport {
	args := map[string]any{"task_id": taskID}
	if patch {
		args["patch"] = true
	}
	res := callTool(ctx, e.Session, "agent_task_diff", args)
	if res.StructuredContent == nil {
		failf("FAIL: agent_task_diff returned no structured content:\n%s", indent(textContent(res)))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		failf("FAIL: agent_task_diff result could not be re-encoded: %v", err)
	}
	var rep worktreeReport
	if err := json.Unmarshal(b, &rep); err != nil {
		failf("FAIL: agent_task_diff result did not parse: %v\n%s", err, indent(string(b)))
	}
	return rep
}

// worktreeLists reports whether the diff names this file. Paths come back
// relative to the repository root, so the base name is what can be matched
// without assuming where in the tree the file sits.
func worktreeLists(rep worktreeReport, name string) bool {
	for _, f := range rep.Files {
		if filepath.Base(filepath.FromSlash(f.Path)) == name {
			return true
		}
	}
	return false
}

// gitOut runs git in dir and returns its combined output, which is what makes a
// failure readable — git says why on stderr.
func gitOut(dir string, args ...string) (string, error) {
	// winspawn.Harden: este escenario hace decenas de llamadas a git, y sin esto
	// son decenas de parpadeos (issue #18). Es el mismo defecto que gitx.run.
	cmd := winspawn.Harden(exec.Command("git", args...))
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\r\n"), err
}

// mustGitOut is gitOut for the reads this scenario cannot continue without.
func mustGitOut(failf func(string, ...any), dir string, args ...string) string {
	out, err := gitOut(dir, args...)
	if err != nil {
		failf("FAIL: git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, indent(out))
	}
	return out
}

func statusLines(status string) []string {
	if strings.TrimSpace(status) == "" {
		return nil
	}
	return splitLines(status)
}

// samePath compares two directories as the filesystem would. Windows is
// case-insensitive, and the server may well spell a path differently from the
// way SMOKE_CWD did.
func samePath(a, b string) bool {
	ea, err := filepath.EvalSymlinks(a)
	if err == nil {
		a = ea
	}
	eb, err := filepath.EvalSymlinks(b)
	if err == nil {
		b = eb
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
