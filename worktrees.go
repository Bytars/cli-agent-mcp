// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Bytars/cli-agent-mcp/internal/config"
	"github.com/Bytars/cli-agent-mcp/internal/gitx"
	"github.com/Bytars/cli-agent-mcp/internal/task"
)

// maxPatchBytes bounds a returned diff. A patch is for reading, and one large
// enough to blow a context window is not being read by anyone.
const maxPatchBytes = 60_000

// resolveWorkspace decides where a task's worker will actually run: the
// directory the caller asked for, or a git worktree cut for this task alone.
//
// Isolation is opt-in per call rather than a mode, because it is the right
// answer often but not always: a worktree is a fresh checkout, so anything the
// repository does not track — node_modules, a .env, a build cache — is not
// there, and a task that needs those should run in place.
func resolveWorkspace(ctx context.Context, cfg config.Config, cwd string, isolate bool) (task.Workspace, string, error) {
	if !isolate {
		return task.At(cwd), "", nil
	}
	ws, err := task.NewWorktree(ctx, cwd, task.WorktreeRoot(cfg.WorktreeDir, cfg.StateDir))
	if err != nil {
		return task.Workspace{}, err.Error(), err
	}
	return ws, "", nil
}

// workspaceNote tells the caller their task is not running where they asked,
// which is otherwise invisible until someone looks for changes in the original
// directory and finds none.
func workspaceNote(snap task.Snapshot) string {
	if snap.Worktree == "" {
		return ""
	}
	return fmt.Sprintf("\n\nThis task ran isolated in %s, on branch %s (cut from %s). "+
		"Its changes are NOT in the original working copy — review them with agent_task_diff, "+
		"merge the branch when you want them, and call agent_remove_worktree to clean up.",
		snap.Worktree, snap.Branch, snap.Repo)
}

type diffInput struct {
	TaskID string `json:"task_id" jsonschema:"The task whose changes to summarize."`
	Patch  bool   `json:"patch,omitempty" jsonschema:"Include the actual diff, not just the list of files. Truncated if very large."`
}

type cleanupInput struct {
	TaskID string `json:"task_id" jsonschema:"The task whose isolated worktree to delete."`
	Force  bool   `json:"force,omitempty" jsonschema:"Delete even when the worktree still holds uncommitted changes. That work exists nowhere else, so ask the user before setting this."`
}

// registerWorktreeTools adds the two tools that make an isolated run reviewable:
// what it changed, and cleaning up after it.
func registerWorktreeTools(srv *mcp.Server, mgr *task.Manager) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "agent_task_diff",
		Description: "Show what a task actually changed, compared against where the repository stood when it started. " +
			"Use it to review a delegated task's work instead of taking its word for it, and always for a task that ran isolated — its changes are in a worktree of its own, so they are not in the directory you asked about. " +
			"Set patch=true for the diff itself rather than just the file list.",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in diffInput) (*mcp.CallToolResult, gitx.Report, error) {
		t, ok := mgr.Get(in.TaskID)
		if !ok {
			return errResult(fmt.Sprintf("unknown task %q", in.TaskID)), gitx.Report{}, nil
		}
		if _, ok := gitx.Available(); !ok {
			return errResult("git was not found on this machine, so changes cannot be summarized"), gitx.Report{}, nil
		}
		snap := t.Snapshot()
		rep, err := gitx.Summarize(ctx, snap.Cwd, snap.BaseCommit, in.Patch, maxPatchBytes)
		if err != nil {
			return errResult(err.Error()), gitx.Report{}, nil
		}
		return textResult(rep.Text()), rep, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "agent_remove_worktree",
		Description: "Delete the isolated worktree a task ran in, along with its branch. " +
			"Refuses while the worktree still holds uncommitted changes — that work is the whole product of the task and exists nowhere else — unless force is set. " +
			"Review with agent_task_diff and merge the branch first if you want to keep the work.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: ptr(true),
			IdempotentHint:  true,
			OpenWorldHint:   ptr(false),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in cleanupInput) (*mcp.CallToolResult, task.Snapshot, error) {
		path, err := mgr.RemoveWorktree(ctx, in.TaskID, in.Force)
		if err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		t, _ := mgr.Get(in.TaskID)
		return textResult("Removed the worktree at " + path + " and its branch."), t.Snapshot(), nil
	})
}
