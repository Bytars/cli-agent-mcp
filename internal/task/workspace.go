// SPDX-License-Identifier: Apache-2.0

package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Bytars/cli-agent-mcp/internal/gitx"
)

// Workspace is where a task's worker actually runs.
//
// Most of the time it is just the requested directory. When a task asks to be
// isolated it is a git worktree instead: a checkout of its own, on a branch of
// its own, sharing the repository's history. That distinction matters as soon
// as more than one worker is running, because agents edit files — two of them
// in one checkout overwrite each other and produce a diff neither intended.
type Workspace struct {
	// Path is the directory the worker runs in.
	Path string

	// Repo and Branch are set only for a worktree: the repository it was cut
	// from, and the branch created for it. Repo is what worktree removal has to
	// be run from, since the worktree itself is what is being removed.
	Repo   string
	Branch string
}

// Isolated reports whether this workspace is a worktree of its own rather than
// the caller's directory.
func (w Workspace) Isolated() bool { return w.Repo != "" }

// At returns a plain workspace: the worker runs directly in dir.
func At(dir string) Workspace { return Workspace{Path: dir} }

// NewWorktree cuts an isolated checkout of the repository containing cwd.
//
// The worktree is created under root rather than inside the repository, so it
// never shows up as untracked clutter in the very diff it exists to produce.
//
// It names itself rather than taking the task id, because the worktree has to
// exist before the task does — the task records where its worker runs, and that
// is decided here. The name is carried on the snapshot either way.
func NewWorktree(ctx context.Context, cwd, root string) (Workspace, error) {
	var tok [4]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return Workspace{}, fmt.Errorf("naming the worktree: %w", err)
	}
	// The prefix makes these obviously machine-made, so a person listing
	// branches can tell at a glance which came from a delegated task.
	name := "cli-agent-" + hex.EncodeToString(tok[:])
	return newWorktreeNamed(ctx, cwd, root, name)
}

func newWorktreeNamed(ctx context.Context, cwd, root, name string) (Workspace, error) {
	if _, ok := gitx.Available(); !ok {
		return Workspace{}, fmt.Errorf("git was not found on this machine, so an isolated worktree cannot be created")
	}
	repo, err := gitx.Root(ctx, cwd)
	if err != nil {
		return Workspace{}, fmt.Errorf("cannot isolate this task: %w", err)
	}
	if head, err := gitx.Head(ctx, repo); err != nil || head == "" {
		return Workspace{}, fmt.Errorf("cannot isolate this task: %s has no commits yet, and a worktree needs something to branch from", repo)
	}

	path := filepath.Join(root, name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("creating the worktree directory %s: %w", root, err)
	}
	if _, err := os.Stat(path); err == nil {
		return Workspace{}, fmt.Errorf("worktree path %s already exists", path)
	}

	branch := "cli-agent/" + name
	if err := gitx.AddWorktree(ctx, repo, path, branch); err != nil {
		return Workspace{}, err
	}
	return Workspace{Path: path, Repo: repo, Branch: branch}, nil
}

// Workspace reports where this task's worker runs.
func (t *Task) Workspace() Workspace {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.workspace
}

// RemoveWorktree tears down a task's isolated checkout.
//
// It refuses a worktree that still holds uncommitted work unless force is set:
// that work is the entire product of the task, and it exists nowhere else.
func (m *Manager) RemoveWorktree(ctx context.Context, id string, force bool) (string, error) {
	t, ok := m.Get(id)
	if !ok {
		return "", fmt.Errorf("unknown task %q", id)
	}
	t.mu.Lock()
	ws, running := t.workspace, t.running
	t.mu.Unlock()

	if !ws.Isolated() {
		return "", fmt.Errorf("task %s did not run in an isolated worktree; there is nothing to remove", id)
	}
	if running {
		return "", fmt.Errorf("task %s is still running in %s; cancel it before removing its worktree", id, ws.Path)
	}

	if !force {
		rep, err := gitx.Summarize(ctx, ws.Path, "", false, 0)
		if err == nil && (rep.Dirty || len(rep.Files) > 0) {
			return "", fmt.Errorf("worktree %s still holds uncommitted changes from task %s, and they exist nowhere else. "+
				"Review them with agent_task_diff and commit or copy anything worth keeping, then call again with force=true",
				ws.Path, id)
		}
	}
	if err := gitx.RemoveWorktree(ctx, ws.Repo, ws.Path, ws.Branch, force); err != nil {
		return "", err
	}

	t.mu.Lock()
	t.workspace = Workspace{Path: ws.Path}
	t.mu.Unlock()
	t.persist()

	return ws.Path, nil
}

// WorktreeRoot is where isolated checkouts live when the operator names no
// directory: alongside the task records, so everything this server creates on
// disk is in one place.
func WorktreeRoot(configured, stateDir string) string {
	if s := strings.TrimSpace(configured); s != "" {
		if abs, err := filepath.Abs(s); err == nil {
			return abs
		}
		return s
	}
	if stateDir != "" {
		return filepath.Join(stateDir, "worktrees")
	}
	return filepath.Join(os.TempDir(), "cli-agent-mcp", "worktrees")
}
