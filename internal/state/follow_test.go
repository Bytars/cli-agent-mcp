package state

import (
	"os"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func mustNext(t *testing.T, f *Follower) []string {
	t.Helper()
	lines, err := f.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return lines
}

func TestFollowerReturnsOnlyWhatIsNew(t *testing.T) {
	s := newStore(t)
	const id = "task-1-aaaa"

	if err := s.AppendLine(id, "first"); err != nil {
		t.Fatalf("AppendLine: %v", err)
	}
	f, err := s.Follow(id, 0) // only what arrives from now on
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}
	if got := mustNext(t, f); len(got) != 0 {
		t.Fatalf("expected nothing pending, got %q", got)
	}

	_ = s.AppendLine(id, "second")
	_ = s.AppendLine(id, "third")
	got := mustNext(t, f)
	if len(got) != 2 || got[0] != "second" || got[1] != "third" {
		t.Fatalf("expected the two new lines, got %q", got)
	}
	if got := mustNext(t, f); len(got) != 0 {
		t.Fatalf("a second read with no writes must return nothing, got %q", got)
	}
}

func TestFollowerBacklogIsTheLastNLines(t *testing.T) {
	s := newStore(t)
	const id = "task-2-bbbb"
	for _, l := range []string{"a", "b", "c", "d", "e"} {
		_ = s.AppendLine(id, l)
	}

	f, err := s.Follow(id, 2)
	if err != nil {
		t.Fatalf("Follow: %v", err)
	}
	got := mustNext(t, f)
	if len(got) != 2 || got[0] != "d" || got[1] != "e" {
		t.Fatalf("expected [d e], got %q", got)
	}
}

func TestFollowerBacklogLargerThanTheFileYieldsEverything(t *testing.T) {
	s := newStore(t)
	const id = "task-3-cccc"
	_ = s.AppendLine(id, "only-one")

	f, _ := s.Follow(id, 500)
	got := mustNext(t, f)
	if len(got) != 1 || got[0] != "only-one" {
		t.Fatalf("expected [only-one], got %q", got)
	}
}

// A transcript line may itself contain newlines — a tool result, a stack trace.
// The store escapes them so one stored line stays one transcript line; a
// follower that did not reverse that would report a single event as several.
func TestFollowerRestoresEmbeddedNewlines(t *testing.T) {
	s := newStore(t)
	const id = "task-4-dddd"
	_ = s.AppendLine(id, "line1\nline2\\n-literal")

	f, _ := s.Follow(id, -1)
	got := mustNext(t, f)
	if len(got) != 1 {
		t.Fatalf("expected a single line, got %d: %q", len(got), got)
	}
	if got[0] != "line1\nline2\\n-literal" {
		t.Fatalf("line restored wrong: %q", got[0])
	}
}

// The writer appends; a reader can arrive mid-line. Emitting that fragment would
// show half an event and then repeat it whole on the next poll.
func TestFollowerHoldsBackAnIncompleteLine(t *testing.T) {
	s := newStore(t)
	const id = "task-5-eeee"
	path := s.taskPath(id, ".log")

	if err := os.WriteFile(path, []byte("complete\nincomp"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, _ := s.Follow(id, -1)
	got := mustNext(t, f)
	if len(got) != 1 || got[0] != "complete" {
		t.Fatalf("expected only the complete line, got %q", got)
	}

	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_, _ = fh.WriteString("lete\n")
	fh.Close()

	got = mustNext(t, f)
	if len(got) != 1 || got[0] != "incomplete" {
		t.Fatalf("expected [incomplete] once it was finished, got %q", got)
	}
}

// A shrinking file is a different file: an id whose store was pruned and
// rebuilt. Reading on from the old offset would splice two transcripts.
func TestFollowerRestartsWhenTheFileIsReplaced(t *testing.T) {
	s := newStore(t)
	const id = "task-6-ffff"
	for i := 0; i < 20; i++ {
		_ = s.AppendLine(id, "old")
	}
	f, _ := s.Follow(id, 0)

	s.Forget(id)
	_ = s.AppendLine(id, "new")

	got := mustNext(t, f)
	if len(got) != 1 || got[0] != "new" {
		t.Fatalf("expected [new] after the file was replaced, got %q", got)
	}
}

func TestFollowerOnMissingTranscriptIsNotAnError(t *testing.T) {
	s := newStore(t)
	f, err := s.Follow("task-7-0000", 100)
	if err != nil {
		t.Fatalf("Follow on a task with no output: %v", err)
	}
	if got := mustNext(t, f); len(got) != 0 {
		t.Fatalf("expected nothing, got %q", got)
	}
}

func TestFollowRejectsAnUnusableID(t *testing.T) {
	s := newStore(t)
	if _, err := s.Follow("../escape", 0); err == nil {
		t.Fatal("an id that escapes the directory must be rejected")
	}
}

func TestOwnerReadsTheLockWithoutTakingIt(t *testing.T) {
	s := newStore(t)
	if o := s.Owner(); o != nil {
		t.Fatalf("with no lock there must be no owner, got %+v", o)
	}
	if _, err := s.Acquire(); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	o := s.Owner()
	if o == nil || o.PID != os.Getpid() {
		t.Fatalf("expected our own pid, got %+v", o)
	}

	// Reading twice must not disturb the record: a viewer that rewrote the lock
	// would leave the next real server thinking it was alone.
	before, err := os.ReadFile(s.dir + string(os.PathSeparator) + lockFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	_ = s.Owner()
	after, _ := os.ReadFile(s.dir + string(os.PathSeparator) + lockFile)
	if string(before) != string(after) {
		t.Fatal("Owner modified the lock; it must be read-only")
	}
}
