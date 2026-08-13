package inspect

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bytars/cli-agent-mcp/internal/task"
)

func TestTasksEndpointReportsTheDirectoryAndTheTasks(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, snapshot("task-1-aaaa", "claude", task.StatusRunning, "2026-01-02T03:04:05Z"), "one")
	src := openSource(t, dir)

	rec := httptest.NewRecorder()
	src.handleTasks(rec, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var body tasksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable response: %v", err)
	}
	if body.Dir == "" {
		t.Fatal("the response must say which directory it is reading")
	}
	if len(body.Tasks) != 1 || body.Tasks[0].ID != "task-1-aaaa" {
		t.Fatalf("unexpected tasks: %+v", body.Tasks)
	}
}

// The page appends deltas rather than re-fetching a growing transcript, so
// `total` has to come back as a usable next cursor.
func TestLogEndpointServesASliceAndTheNextCursor(t *testing.T) {
	dir := t.TempDir()
	snap := snapshot("task-1-aaaa", "claude", task.StatusRunning, "2026-01-02T03:04:05Z")
	seed(t, dir, snap, "[stderr] one", "[stderr] two")
	src := openSource(t, dir)

	rec := httptest.NewRecorder()
	src.handleLog(rec, httptest.NewRequest(http.MethodGet, "/api/log?id=task-1-aaaa&since=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var body logResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable response: %v", err)
	}
	if body.Total != 2 {
		t.Fatalf("total = %d, expected 2", body.Total)
	}
	if len(body.Lines) != 1 {
		t.Fatalf("from since=1 expected one line, got %q", body.Lines)
	}
	if body.Task.ID != "task-1-aaaa" {
		t.Fatalf("the response must carry the task record, it carried %+v", body.Task)
	}
}

func TestLogEndpointRejectsAnUnknownTask(t *testing.T) {
	src := openSource(t, t.TempDir())
	rec := httptest.NewRecorder()
	src.handleLog(rec, httptest.NewRequest(http.MethodGet, "/api/log?id=task-9-zzzz", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestIndexOnlyAnswersTheRoot(t *testing.T) {
	src := openSource(t, t.TempDir())
	rec := httptest.NewRecorder()
	src.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/something-else", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 outside the root, got %d", rec.Code)
	}
}

// Binding beyond localhost exposes full transcripts with no authentication, so
// the check that gates it must not be generous.
func TestOnlyRealLoopbackAddressesCountAsLocal(t *testing.T) {
	local := []string{"127.0.0.1", "localhost", "::1", "[::1]", "127.10.20.30"}
	for _, h := range local {
		if !isLoopback(h) {
			t.Fatalf("%q should count as local", h)
		}
	}
	remote := []string{"", "0.0.0.0", "192.168.1.50", "::", "example.com"}
	for _, h := range remote {
		if isLoopback(h) {
			t.Fatalf("%q should NOT count as local", h)
		}
	}
}
