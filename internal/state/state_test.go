// SPDX-License-Identifier: Apache-2.0

package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

type rec struct {
	ID     string `json:"task_id"`
	Status string `json:"status"`
}

func TestTaskRecordSurvivesRoundTrip(t *testing.T) {
	s := open(t)
	if err := s.SaveTask("t-1", rec{ID: "t-1", Status: "running"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// A later save must replace the record, not accumulate a second one.
	if err := s.SaveTask("t-1", rec{ID: "t-1", Status: "done"}); err != nil {
		t.Fatalf("resave: %v", err)
	}

	records, err := s.LoadTasks()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	var got rec
	if err := json.Unmarshal(records[0], &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "done" {
		t.Errorf("status = %q, want the latest save %q", got.Status, "done")
	}
}

// A transcript line can contain newlines and backslashes. If the escaping is
// not exactly reversible, the restored line index stops matching the live one
// and every since_line read returns the wrong slice.
func TestTranscriptLinesRoundTripExactly(t *testing.T) {
	s := open(t)
	want := []string{
		`{"type":"text","text":"hello"}`,
		"line with\nan embedded newline",
		`a literal \n that is not a newline`,
		`windows path C:\Users\someone\Documents`,
		"[stderr] warning: something",
	}
	for _, l := range want {
		if err := s.AppendLine("t-1", l); err != nil {
			t.Fatalf("append %q: %v", l, err)
		}
	}
	got, err := s.ReadLines("t-1")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

func TestReadLinesOnUnknownTaskIsEmpty(t *testing.T) {
	s := open(t)
	lines, err := s.ReadLines("never-existed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("got %d lines, want none", len(lines))
	}
}

// A truncated or hand-mangled record must cost only itself.
func TestCorruptRecordDoesNotHideTheOthers(t *testing.T) {
	s := open(t)
	if err := s.SaveTask("t-good", rec{ID: "t-good", Status: "done"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	bad := filepath.Join(s.Dir(), tasksSubdir, "t-bad.json")
	if err := os.WriteFile(bad, []byte(`{"task_id": "t-bad"`), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	records, err := s.LoadTasks()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want just the good one", len(records))
	}
}

func TestForgetAndPruneRemoveBothFiles(t *testing.T) {
	s := open(t)
	for _, id := range []string{"t-1", "t-2", "t-3"} {
		if err := s.SaveTask(id, rec{ID: id}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		if err := s.AppendLine(id, "some output"); err != nil {
			t.Fatalf("append %s: %v", id, err)
		}
		// Distinct mod times, so Prune's newest-wins ordering is deterministic.
		time.Sleep(10 * time.Millisecond)
	}

	s.Forget("t-1")
	for _, ext := range []string{".json", ".log"} {
		if _, err := os.Stat(filepath.Join(s.Dir(), tasksSubdir, "t-1"+ext)); !os.IsNotExist(err) {
			t.Errorf("Forget left %s behind", ext)
		}
	}

	s.Prune(1)
	records, err := s.LoadTasks()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("after Prune(1) got %d records, want 1", len(records))
	}
	var got rec
	json.Unmarshal(records[0], &got)
	if got.ID != "t-3" {
		t.Errorf("Prune kept %q, want the newest %q", got.ID, "t-3")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), tasksSubdir, "t-2.log")); !os.IsNotExist(err) {
		t.Error("Prune removed the record but left its transcript")
	}
}

// The whole point of the lock: a second instance must be able to tell the first
// one is still alive, because it owns tasks this one cannot see.
func TestAcquireReportsALiveInstance(t *testing.T) {
	s := open(t)

	prev, err := s.Acquire()
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if prev != nil {
		t.Fatalf("an empty state dir reported a previous owner: %+v", prev)
	}

	// Our own PID is skipped, so simulate a different live process with the
	// parent's — alive by definition while this test runs.
	live := Owner{PID: os.Getppid(), Started: time.Now(), Exe: "other-instance"}
	buf, _ := json.Marshal(live)
	if err := os.WriteFile(filepath.Join(s.Dir(), lockFile), buf, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	prev, err = s.Acquire()
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if prev == nil {
		t.Fatal("a live previous owner was not reported")
	}
	if prev.PID != live.PID {
		t.Errorf("reported pid %d, want %d", prev.PID, live.PID)
	}

	// Acquiring must also take ownership, or the next instance would keep
	// blaming a process that has since exited.
	after, _ := os.ReadFile(filepath.Join(s.Dir(), lockFile))
	var now Owner
	json.Unmarshal(after, &now)
	if now.PID != os.Getpid() {
		t.Errorf("lock still owned by pid %d, want this process %d", now.PID, os.Getpid())
	}
}

func TestStaleLockIsNotReported(t *testing.T) {
	s := open(t)
	// PID 0 is never a live user process, standing in for an owner that exited.
	buf, _ := json.Marshal(Owner{PID: 0, Started: time.Now(), Exe: "dead"})
	if err := os.WriteFile(filepath.Join(s.Dir(), lockFile), buf, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	prev, err := s.Acquire()
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if prev != nil {
		t.Errorf("a dead owner was reported as a conflict: %+v", prev)
	}
}

// An id that walked out of the tasks directory would let a malformed record
// overwrite arbitrary files.
func TestPathTraversalIsRejected(t *testing.T) {
	s := open(t)
	for _, id := range []string{"../escape", `..\escape`, "a/b", ".hidden", ""} {
		if err := s.SaveTask(id, rec{ID: id}); err == nil {
			t.Errorf("SaveTask accepted unsafe id %q", id)
		}
		if err := s.AppendLine(id, "x"); err == nil {
			t.Errorf("AppendLine accepted unsafe id %q", id)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Dir(), tasksSubdir)); len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("unsafe ids created files: %s", strings.Join(names, ", "))
	}
}

// The cancel request is a file named from a task id that arrives over the wire,
// so the same guard that protects the record and the transcript has to cover it.
// Without that, an id of "../../server" would put an attacker-chosen path under
// this process's control.
func TestCancelRequestsRefuseUnusableIDs(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for _, id := range []string{"", "../escape", `..\escape`, "a/b", ".hidden"} {
		if err := s.RequestCancel(id); err == nil {
			t.Errorf("RequestCancel(%q) was accepted; it must be refused", id)
		}
		if s.CancelRequested(id) {
			t.Errorf("CancelRequested(%q) reported true for an id that cannot be stored", id)
		}
		if err := s.ClearCancel(id); err == nil {
			t.Errorf("ClearCancel(%q) was accepted; it must be refused", id)
		}
	}
}

// The three calls have to agree with each other, or a request would be written
// where nothing looks for it.
func TestCancelRequestRoundTrips(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	const id = "task-1-abcd"
	if s.CancelRequested(id) {
		t.Fatal("a fresh store reported a request nobody made")
	}
	if err := s.RequestCancel(id); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if !s.CancelRequested(id) {
		t.Error("the request was written but is not visible")
	}
	if err := s.ClearCancel(id); err != nil {
		t.Fatalf("ClearCancel: %v", err)
	}
	if s.CancelRequested(id) {
		t.Error("the request survived being cleared")
	}
	// Clearing what is not there is how the owner starts every turn, so it must
	// not be an error.
	if err := s.ClearCancel(id); err != nil {
		t.Errorf("clearing an absent request: %v", err)
	}
}
