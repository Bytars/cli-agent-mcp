// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDisabledLoggerIsSafe(t *testing.T) {
	var l *Logger // nil
	l.Log("x", map[string]any{"a": 1})
	if err := l.Close(); err != nil {
		t.Fatalf("Close on nil logger: %v", err)
	}

	l2, err := New("") // empty path → disabled
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if l2.Enabled() {
		t.Error("empty-path logger should be disabled")
	}
	l2.Log("x", nil) // must not panic or write
}

// A missing parent directory must be created, not fatal — this is the footgun
// that would otherwise stop the server from starting.
func TestCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "audit.jsonl")
	l, err := New(path)
	if err != nil {
		t.Fatalf("New with missing parent dir: %v", err)
	}
	defer l.Close()
	if !l.Enabled() {
		t.Fatal("logger should be enabled")
	}

	l.Log("turn_start", map[string]any{"task_id": "t1", "prompt": "hi"})
	l.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("audit file not created: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatal("no audit record written")
	}
	var rec map[string]any
	if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
		t.Fatalf("record is not valid JSON: %v", err)
	}
	for _, k := range []string{"ts", "event", "task_id", "prompt"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("record missing %q: %v", k, rec)
		}
	}
	if rec["event"] != "turn_start" {
		t.Errorf("event = %v, want turn_start", rec["event"])
	}
}
