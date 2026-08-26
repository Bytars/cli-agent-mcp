// SPDX-License-Identifier: Apache-2.0

package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo builds a real repository with one commit. Everything here is
// exercised against actual git rather than a fake, because the whole point of
// shelling out is to see the repository the way the worker's own git does.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, ok := Available(); !ok {
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
	write(t, dir, "a.txt", "one\ntwo\nthree\n")
	commit(t, dir, "initial")
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// The case the whole design turns on: a worker that commits its own work. A
// diff against HEAD would report nothing changed, which reads as "the agent did
// not touch anything" — the opposite of what happened.
func TestSummarizeSeesWorkTheAgentAlreadyCommitted(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)

	base, err := Head(ctx, dir)
	if err != nil || base == "" {
		t.Fatalf("Head: %q %v", base, err)
	}

	write(t, dir, "a.txt", "one\ntwo\nthree\nfour\n")
	write(t, dir, "b.txt", "new file\n")
	commit(t, dir, "the worker's commit")

	rep, err := Summarize(ctx, dir, base, true, 10_000)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(rep.Commits) != 1 || !strings.Contains(rep.Commits[0], "the worker's commit") {
		t.Errorf("Commits = %v, want the one commit made since base", rep.Commits)
	}
	if len(rep.Files) != 2 {
		t.Errorf("Files = %+v, want both changed files", rep.Files)
	}
	if rep.Added < 2 {
		t.Errorf("lines added = %d, want at least the two new lines", rep.Added)
	}
	if rep.Dirty {
		t.Error("Dirty = true, but everything was committed")
	}
	if !strings.Contains(rep.Patch, "four") {
		t.Errorf("patch does not contain the change:\n%s", rep.Patch)
	}
}

// A file the worker created and never staged is still a change to the
// repository, and it never appears in a diff.
func TestSummarizeReportsUntrackedFiles(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	base, _ := Head(ctx, dir)

	write(t, dir, "scratch.txt", "created but never staged\n")

	rep, err := Summarize(ctx, dir, base, false, 0)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	found := false
	for _, f := range rep.Files {
		if f.Path == "scratch.txt" && f.Status == "untracked" {
			found = true
		}
	}
	if !found {
		t.Errorf("untracked file missing from %+v", rep.Files)
	}
	if !rep.Dirty {
		t.Error("Dirty = false with an untracked file present")
	}
}

func TestSummarizeOnAnUntouchedRepoSaysSo(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	base, _ := Head(ctx, dir)

	rep, err := Summarize(ctx, dir, base, false, 0)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(rep.Files) != 0 || len(rep.Commits) != 0 {
		t.Errorf("expected no changes, got %+v / %v", rep.Files, rep.Commits)
	}
	if !strings.Contains(rep.Text(), "No changes") {
		t.Errorf("text should say plainly that nothing changed:\n%s", rep.Text())
	}
}

// Outside a repository there is nothing to compare, and saying so is better
// than reporting an empty diff that looks like "nothing changed".
func TestSummarizeOutsideARepoIsAnError(t *testing.T) {
	if _, ok := Available(); !ok {
		t.Skip("git is not installed")
	}
	if _, err := Summarize(context.Background(), t.TempDir(), "", false, 0); err == nil {
		t.Error("expected an error outside a git repository")
	}
}

func TestHeadOnAnEmptyRepoIsNotAnError(t *testing.T) {
	if _, ok := Available(); !ok {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	head, err := Head(context.Background(), dir)
	if err != nil {
		t.Errorf("a repository with no commits is a legitimate state, got error: %v", err)
	}
	if head != "" {
		t.Errorf("Head = %q, want empty on an unborn branch", head)
	}
}
