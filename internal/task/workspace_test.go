// SPDX-License-Identifier: Apache-2.0

package task

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bytars/cli-agent-mcp/internal/agent"
	"github.com/Bytars/cli-agent-mcp/internal/gitx"
)

func repoWithOneCommit(t *testing.T) string {
	t.Helper()
	if _, ok := gitx.Available(); !ok {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

func TestNewWorktreeGivesTheTaskACheckoutOfItsOwn(t *testing.T) {
	ctx := context.Background()
	repo := repoWithOneCommit(t)
	root := filepath.Join(t.TempDir(), "worktrees")

	ws, err := NewWorktree(ctx, repo, root)
	if err != nil {
		t.Fatalf("NewWorktree: %v", err)
	}
	if !ws.Isolated() {
		t.Fatal("workspace does not report itself isolated")
	}
	if ws.Path == repo {
		t.Error("the worktree must not be the repository itself")
	}
	// It lives outside the repository, so it never appears as untracked clutter
	// in the very diff it exists to produce.
	if strings.HasPrefix(ws.Path, repo+string(filepath.Separator)) {
		t.Errorf("worktree %s was created inside the repository %s", ws.Path, repo)
	}
	if _, err := os.Stat(filepath.Join(ws.Path, "a.txt")); err != nil {
		t.Errorf("the worktree is missing the repository's content: %v", err)
	}
	if branch := gitx.Branch(ctx, ws.Path); branch != ws.Branch {
		t.Errorf("worktree is on branch %q, want the one created for it (%q)", branch, ws.Branch)
	}
}

// Two isolated tasks must not land in the same directory or on the same branch;
// that would defeat the only reason worktrees are here.
func TestWorktreesDoNotCollide(t *testing.T) {
	ctx := context.Background()
	repo := repoWithOneCommit(t)
	root := filepath.Join(t.TempDir(), "worktrees")

	a, err := NewWorktree(ctx, repo, root)
	if err != nil {
		t.Fatalf("first worktree: %v", err)
	}
	b, err := NewWorktree(ctx, repo, root)
	if err != nil {
		t.Fatalf("second worktree: %v", err)
	}
	if a.Path == b.Path || a.Branch == b.Branch {
		t.Errorf("two worktrees collided: %+v vs %+v", a, b)
	}
}

func TestNewWorktreeOutsideARepoIsRefused(t *testing.T) {
	if _, ok := gitx.Available(); !ok {
		t.Skip("git is not installed")
	}
	_, err := NewWorktree(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "wt"))
	if err == nil {
		t.Error("expected isolation to be refused outside a git repository")
	}
}

// The uncommitted work in a worktree is the entire product of the task and
// exists nowhere else, so removing it has to be a deliberate act.
func TestRemoveWorktreeRefusesToDiscardWork(t *testing.T) {
	ctx := context.Background()
	repo := repoWithOneCommit(t)
	root := filepath.Join(t.TempDir(), "worktrees")

	ws, err := NewWorktree(ctx, repo, root)
	if err != nil {
		t.Fatalf("NewWorktree: %v", err)
	}

	m := NewManager(10)
	tk := &Task{ID: "t1", status: StatusDone, workspace: ws, Cwd: ws.Path}
	m.tasks = map[string]*Task{"t1": tk}
	m.order = []string{"t1"}

	if err := os.WriteFile(filepath.Join(ws.Path, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.RemoveWorktree(ctx, "t1", false); err == nil {
		t.Fatal("removing a worktree with uncommitted changes must be refused")
	} else if !strings.Contains(err.Error(), "force") {
		t.Errorf("the refusal should say how to proceed deliberately, got: %v", err)
	}

	if _, err := m.RemoveWorktree(ctx, "t1", true); err != nil {
		t.Fatalf("forced removal: %v", err)
	}
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Errorf("worktree still present after a forced removal: %v", err)
	}
	if tk.Workspace().Isolated() {
		t.Error("the task still reports an isolated workspace after its worktree was removed")
	}
}

func TestRemoveWorktreeOnAPlainTaskSaysSo(t *testing.T) {
	m := NewManager(10)
	m.tasks = map[string]*Task{"t1": {ID: "t1", status: StatusDone, workspace: At("/tmp/x")}}
	m.order = []string{"t1"}

	if _, err := m.RemoveWorktree(context.Background(), "t1", false); err == nil {
		t.Error("expected an error for a task that never ran isolated")
	}
}

// A task's changes have to remain reviewable from the worktree it ran in, since
// that is the only place they exist.
func TestIsolatedTaskRecordsWhereItRan(t *testing.T) {
	ctx := context.Background()
	repo := repoWithOneCommit(t)
	root := filepath.Join(t.TempDir(), "worktrees")

	ws, err := NewWorktree(ctx, repo, root)
	if err != nil {
		t.Fatalf("NewWorktree: %v", err)
	}
	m := NewManager(10)
	snap := m.newTask(agent.NewMockAdapter(), ws, agent.RunSpec{Prompt: "x"}).Snapshot()

	if snap.Worktree != ws.Path || snap.Branch != ws.Branch || snap.Repo != ws.Repo {
		t.Errorf("snapshot lost the workspace: %+v", snap)
	}
	if snap.BaseCommit == "" {
		t.Error("base commit was not captured, so the task's changes cannot be summarized later")
	}
}
