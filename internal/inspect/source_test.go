package inspect

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Bytars/cli-agent-mcp/internal/state"
	"github.com/Bytars/cli-agent-mcp/internal/task"
)

// seed writes a task exactly the way the server does: one JSON record plus a
// transcript appended line by line.
func seed(t *testing.T, dir string, snap task.Snapshot, lines ...string) {
	t.Helper()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer store.Close()
	if err := store.SaveTask(snap.ID, snap); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}
	for _, l := range lines {
		if err := store.AppendLine(snap.ID, l); err != nil {
			t.Fatalf("AppendLine: %v", err)
		}
	}
}

func snapshot(id, agent string, status task.Status, started string) task.Snapshot {
	return task.Snapshot{
		ID:        id,
		Agent:     agent,
		Cwd:       "C:\\project",
		Status:    status,
		StartedAt: started,
		Prompts:   []string{"fix the login"},
	}
}

func openSource(t *testing.T, dir string) *Source {
	t.Helper()
	src, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(src.Close)
	return src
}

func TestTasksComeBackNewestFirst(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, snapshot("task-1-aaaa", "claude", task.StatusDone, "2026-01-02T03:04:05Z"))
	seed(t, dir, snapshot("task-2-bbbb", "claude", task.StatusRunning, "2026-01-02T05:00:00Z"))
	seed(t, dir, snapshot("task-3-cccc", "cursor", task.StatusFailed, "2026-01-02T04:00:00Z"))

	got, err := openSource(t, dir).Tasks()
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	want := []string{"task-2-bbbb", "task-3-cccc", "task-1-aaaa"}
	if len(got) != len(want) {
		t.Fatalf("expected %d tasks, got %d", len(want), len(got))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("unexpected order: %v", []string{got[0].ID, got[1].ID, got[2].ID})
		}
	}
}

func TestResolveAcceptsIDFragmentsAndAliases(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, snapshot("task-1-aaaa", "claude", task.StatusDone, "2026-01-02T03:04:05Z"))
	seed(t, dir, snapshot("task-2-bbbb", "claude", task.StatusRunning, "2026-01-02T05:00:00Z"))
	src := openSource(t, dir)

	cases := map[string]string{
		"task-2-bbbb": "task-2-bbbb", // exact
		"bbbb":        "task-2-bbbb", // fragment
		"latest":      "task-2-bbbb", // newest
		"running":     "task-2-bbbb", // the only one in flight
		"aaaa":        "task-1-aaaa",
	}
	for ref, want := range cases {
		got, err := src.Resolve(ref)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", ref, err)
		}
		if got.ID != want {
			t.Fatalf("Resolve(%q) = %s, expected %s", ref, got.ID, want)
		}
	}
}

func TestResolveRefusesAnAmbiguousFragment(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, snapshot("task-1-abcd", "claude", task.StatusDone, "2026-01-02T03:04:05Z"))
	seed(t, dir, snapshot("task-2-abce", "claude", task.StatusDone, "2026-01-02T04:04:05Z"))

	_, err := openSource(t, dir).Resolve("abc")
	if err == nil {
		t.Fatal("a fragment matching two tasks must fail, not pick one")
	}
	if !strings.Contains(err.Error(), "task-1-abcd") || !strings.Contains(err.Error(), "task-2-abce") {
		t.Fatalf("the error must name the candidates, it said: %v", err)
	}
}

func TestResolveRefusesToGuessAmongSeveralRunning(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, snapshot("task-1-aaaa", "claude", task.StatusRunning, "2026-01-02T03:04:05Z"))
	seed(t, dir, snapshot("task-2-bbbb", "claude", task.StatusRunning, "2026-01-02T04:04:05Z"))

	if _, err := openSource(t, dir).Resolve("running"); err == nil {
		t.Fatal("with two tasks running, \"running\" is ambiguous and must fail")
	}
}

// Lines is what the web viewer polls: `total` is the cursor for the next call,
// and indices must keep addressing the same lines as the transcript grows.
func TestLinesAreIncrementalWithStableIndices(t *testing.T) {
	dir := t.TempDir()
	snap := snapshot("task-1-aaaa", "claude", task.StatusRunning, "2026-01-02T03:04:05Z")
	seed(t, dir, snap, "one", "two")
	src := openSource(t, dir)

	lines, total, err := src.Lines(snap.ID, 0)
	if err != nil {
		t.Fatalf("Lines: %v", err)
	}
	if total != 2 || len(lines) != 2 || lines[0] != "one" {
		t.Fatalf("first read: total=%d lines=%q", total, lines)
	}

	seed(t, dir, snap, "three")

	lines, total, err = src.Lines(snap.ID, total)
	if err != nil {
		t.Fatalf("Lines incremental: %v", err)
	}
	if total != 3 || len(lines) != 1 || lines[0] != "three" {
		t.Fatalf("the second read should bring only what is new: total=%d lines=%q", total, lines)
	}

	// An old cursor still addresses the same lines.
	lines, _, err = src.Lines(snap.ID, 1)
	if err != nil {
		t.Fatalf("Lines from an old cursor: %v", err)
	}
	if len(lines) != 2 || lines[0] != "two" || lines[1] != "three" {
		t.Fatalf("the indices shifted: %q", lines)
	}
}

func TestLinesRejectsAnIDThatCouldNameAPath(t *testing.T) {
	src := openSource(t, t.TempDir())
	if _, _, err := src.Lines("../../secret", 0); err == nil {
		t.Fatal("an id containing path separators must be rejected")
	}
}

func TestRenderCompactMarksStderrAndRawLeavesLinesAlone(t *testing.T) {
	src := openSource(t, t.TempDir())
	in := []string{"[stderr] could not open", "{\"type\":\"x\"}"}

	if got := src.Render("unknown", in, false); len(got) != 2 || got[0] != in[0] {
		t.Fatalf("raw must return the lines untouched, got %q", got)
	}
	got := src.Render("unknown", in, true)
	if len(got) == 0 || !strings.Contains(got[0], "could not open") {
		t.Fatalf("compact must keep stderr readable, got %q", got)
	}
}

// Go's flag package stops at the first non-flag argument, so `logs task-1 -f`
// would silently ignore -f. Both orders have to work.
func TestFlagsAndPositionalsCanInterleave(t *testing.T) {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	follow := fs.Bool("f", false, "")
	n := fs.Int("n", 0, "")

	got, err := parseWithPositionals(fs, []string{"task-1-aaaa", "-f", "-n", "5"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0] != "task-1-aaaa" {
		t.Fatalf("unexpected positionals: %q", got)
	}
	if !*follow || *n != 5 {
		t.Fatalf("the flags after the positional were lost: f=%v n=%d", *follow, *n)
	}
}

// capture redirects stdout for the duration of fn. The commands print straight
// to stdout, which is right for a CLI and inconvenient for a test.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = orig
	return <-done
}

func TestLogsPrintsTheTranscriptAndFailsWhenTheTaskFailed(t *testing.T) {
	dir := t.TempDir()
	snap := snapshot("task-1-aaaa", "claude", task.StatusFailed, "2026-01-02T03:04:05Z")
	snap.Error = "exit 1: does not compile"
	seed(t, dir, snap, "[stderr] does not compile")

	var code int
	out := capture(t, func() {
		code = runLogs([]string{"aaaa", "--no-follow", "--no-color", "--state-dir", dir})
	})

	if code != 1 {
		t.Fatalf("a failed task must exit with code 1, it exited with %d", code)
	}
	if !strings.Contains(out, "does not compile") {
		t.Fatalf("the output should have included the transcript, it was:\n%s", out)
	}
	if !strings.Contains(out, "task-1-aaaa") {
		t.Fatalf("the output should have identified the task, it was:\n%s", out)
	}
}

func TestTasksListsWhatIsThereAndSaysWhereItLooked(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, snapshot("task-1-aaaa", "claude", task.StatusRunning, "2026-01-02T03:04:05Z"))

	var code int
	out := capture(t, func() {
		code = runTasks([]string{"--no-color", "--state-dir", dir})
	})

	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	for _, want := range []string{dir, "task-1-aaaa", "claude", "running", "fix the login"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q was missing from the output:\n%s", want, out)
		}
	}
}

func TestFormatDurationReadsLikeAStatusLine(t *testing.T) {
	cases := map[string]task.Snapshot{
		"0s":    {StartedAt: "2026-01-02T03:04:05Z", EndedAt: "2026-01-02T03:04:05Z"},
		"45s":   {StartedAt: "2026-01-02T03:04:05Z", EndedAt: "2026-01-02T03:04:50Z"},
		"2m10s": {StartedAt: "2026-01-02T03:04:05Z", EndedAt: "2026-01-02T03:06:15Z"},
		"1h05m": {StartedAt: "2026-01-02T03:04:05Z", EndedAt: "2026-01-02T04:09:05Z"},
	}
	for want, snap := range cases {
		if got := FormatDuration(Elapsed(snap, parseTime(snap.EndedAt))); got != want {
			t.Fatalf("expected %q, got %q", want, got)
		}
	}
}
