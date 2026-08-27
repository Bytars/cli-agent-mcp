// SPDX-License-Identifier: Apache-2.0

// Package gitx is the small amount of git this server needs to answer two
// questions about a delegated task: what did the worker change, and can it be
// given a working directory of its own.
//
// It shells out to git rather than linking a library. The worker is already
// driving the same repository through the same binary, and matching its view
// exactly matters more here than avoiding a process — a library reading the
// index differently from the git the agent just ran would report changes that
// are not there.
//
// Every function is read-only unless its name says otherwise, and every one of
// them degrades to a clear error rather than guessing when the directory is not
// a repository.
package gitx

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bytars/cli-agent-mcp/internal/winspawn"
)

// commandTimeout bounds any single git invocation. A repository on a stalled
// network drive must not hold a tool call open until the client gives up.
const commandTimeout = 30 * time.Second

// Available reports whether git can be found on this machine.
func Available() (string, bool) {
	p, err := exec.LookPath("git")
	return p, err == nil
}

// run executes git in dir and returns its trimmed stdout.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	// winspawn.Harden, o cada llamada a git le parpadea una consola al usuario:
	// este servidor lo lanza una aplicación gráfica sin consola propia, así que
	// git —que es una app de consola— se abre la suya. `run` es el único punto
	// por donde pasan TODAS las invocaciones de git de este paquete, así que
	// envolverlo acá las cubre a todas (issue #18).
	cmd := winspawn.Harden(exec.CommandContext(ctx, "git", args...))
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimRight(out.String(), "\r\n"), nil
}

// Root returns the top level of the repository containing dir.
func Root(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.Clean(out), nil
}

// Head returns the commit dir is currently on. It returns "" with no error in a
// repository that has no commits yet, which is a legitimate state rather than a
// failure — a task can still be started there.
func Head(ctx context.Context, dir string) (string, error) {
	if _, err := Root(ctx, dir); err != nil {
		return "", err
	}
	out, err := run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", nil // unborn branch
	}
	return out, nil
}

// Branch returns the checked-out branch, or "" when HEAD is detached.
func Branch(ctx context.Context, dir string) string {
	out, err := run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || out == "HEAD" {
		return ""
	}
	return out
}

// FileChange is one path the worker touched.
type FileChange struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // human-readable: modified, added, deleted, renamed, untracked
	Added   int    `json:"added,omitempty"`
	Deleted int    `json:"deleted,omitempty"`
}

// Report is everything that changed in a working directory since a base commit.
type Report struct {
	Repo       bool         `json:"repo"`
	Root       string       `json:"root,omitempty"`
	Branch     string       `json:"branch,omitempty"`
	BaseCommit string       `json:"base_commit,omitempty"`
	HeadCommit string       `json:"head_commit,omitempty"`
	Commits    []string     `json:"commits,omitempty"` // made since base, newest first
	Files      []FileChange `json:"files,omitempty"`
	Added      int          `json:"lines_added"`
	Deleted    int          `json:"lines_deleted"`
	Patch      string       `json:"patch,omitempty"`
	Truncated  bool         `json:"patch_truncated,omitempty"`
	Dirty      bool         `json:"dirty"` // uncommitted changes are present
}

// Summarize reports what changed in dir since base.
//
// base is the commit the task started from. Comparing against it — rather than
// against HEAD — is what makes the answer useful when the worker committed its
// own work: a diff against HEAD would show nothing at all and read as "the
// agent changed no files", which is the opposite of the truth.
//
// An empty base falls back to comparing the working tree against HEAD, which is
// all that can be known when the starting point was never recorded.
func Summarize(ctx context.Context, dir, base string, patch bool, maxPatchBytes int) (Report, error) {
	rep := Report{BaseCommit: base}

	root, err := Root(ctx, dir)
	if err != nil {
		return rep, fmt.Errorf("%s is not inside a git repository, so there is nothing to diff: %w", dir, err)
	}
	rep.Repo = true
	rep.Root = root
	rep.Branch = Branch(ctx, dir)
	rep.HeadCommit, _ = Head(ctx, dir)

	// against is what every comparison below is made with: the task's starting
	// commit when we have it, HEAD otherwise.
	against := base
	if against == "" {
		against = "HEAD"
	}
	if base != "" && base != rep.HeadCommit {
		if log, err := run(ctx, dir, "log", "--oneline", "--no-decorate", base+"..HEAD"); err == nil && log != "" {
			rep.Commits = strings.Split(log, "\n")
		}
	}

	// numstat gives per-file line counts; it covers staged and unstaged work in
	// one pass when compared against a commit.
	if out, err := run(ctx, dir, "diff", "--numstat", against); err == nil && out != "" {
		for _, line := range strings.Split(out, "\n") {
			cols := strings.SplitN(line, "\t", 3)
			if len(cols) != 3 {
				continue
			}
			fc := FileChange{Path: cols[2], Status: "modified"}
			fc.Added, _ = strconv.Atoi(cols[0]) // "-" for binary files parses to 0
			fc.Deleted, _ = strconv.Atoi(cols[1])
			rep.Added += fc.Added
			rep.Deleted += fc.Deleted
			rep.Files = append(rep.Files, fc)
		}
	}

	// Untracked files never appear in a diff, and a worker that created a file
	// and did not stage it has still changed the repository.
	if out, err := run(ctx, dir, "ls-files", "--others", "--exclude-standard"); err == nil && out != "" {
		for _, p := range strings.Split(out, "\n") {
			if p = strings.TrimSpace(p); p != "" {
				rep.Files = append(rep.Files, FileChange{Path: p, Status: "untracked"})
			}
		}
	}

	if out, err := run(ctx, dir, "status", "--porcelain"); err == nil {
		rep.Dirty = strings.TrimSpace(out) != ""
	}

	if patch && len(rep.Files) > 0 {
		if out, err := run(ctx, dir, "diff", against); err == nil {
			rep.Patch, rep.Truncated = clip(out, maxPatchBytes)
		}
	}
	return rep, nil
}

// AddWorktree checks out a new branch at path, sharing the repository's history
// but with a working directory of its own.
//
// This is what lets several workers run at once without fighting: agents edit
// files, and two of them in the same checkout will overwrite each other's work
// and produce a diff neither of them intended. A worktree gives each one an
// isolated tree while keeping one git history, so the result can still be
// reviewed and merged normally.
func AddWorktree(ctx context.Context, repo, path, branch string) error {
	if _, err := Root(ctx, repo); err != nil {
		return fmt.Errorf("%s is not inside a git repository, so no worktree can be created there: %w", repo, err)
	}
	if _, err := run(ctx, repo, "worktree", "add", "-b", branch, path); err != nil {
		return err
	}
	return nil
}

// RemoveWorktree deletes a worktree and its branch. Without force it refuses
// when the tree still holds uncommitted work, which is the whole reason the
// worktree existed.
func RemoveWorktree(ctx context.Context, repo, path, branch string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	if _, err := run(ctx, repo, append(args, path)...); err != nil {
		return err
	}
	if branch != "" {
		// -D rather than -d: the branch was created for this task and was never
		// merged anywhere, so git would refuse the safe form every time.
		_, _ = run(ctx, repo, "branch", "-D", branch)
	}
	return nil
}

// clip bounds a patch on a rune boundary, keeping the beginning: unlike a log,
// a diff is read from the top and its first hunks are the ones being reviewed.
func clip(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// Text renders a report the way a person would want to read it.
func (r Report) Text() string {
	if !r.Repo {
		return "Not a git repository, so there is nothing to compare."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s", r.Root)
	if r.Branch != "" {
		fmt.Fprintf(&b, "  (branch %s)", r.Branch)
	}
	b.WriteString("\n")

	if len(r.Files) == 0 && len(r.Commits) == 0 {
		b.WriteString("\nNo changes: the worker left this directory exactly as it found it.")
		return b.String()
	}

	if len(r.Commits) > 0 {
		fmt.Fprintf(&b, "\n%d commit(s) since the task started:\n", len(r.Commits))
		for _, c := range r.Commits {
			fmt.Fprintf(&b, "  %s\n", c)
		}
	}

	if len(r.Files) > 0 {
		fmt.Fprintf(&b, "\n%d file(s) changed, +%d / -%d:\n", len(r.Files), r.Added, r.Deleted)
		for _, f := range r.Files {
			if f.Status == "untracked" {
				fmt.Fprintf(&b, "  %-10s %s\n", "new", f.Path)
				continue
			}
			fmt.Fprintf(&b, "  %-10s %s  (+%d/-%d)\n", f.Status, f.Path, f.Added, f.Deleted)
		}
	}
	if r.Dirty {
		b.WriteString("\nThese changes are uncommitted.\n")
	}
	if r.Patch != "" {
		b.WriteString("\n")
		b.WriteString(r.Patch)
		if r.Truncated {
			b.WriteString("\n… patch truncated.\n")
		}
	}
	return b.String()
}
