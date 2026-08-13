// SPDX-License-Identifier: Apache-2.0

// Package state persists what the server knows across process lifetimes.
//
// The task registry used to live only in RAM. That was fine as long as there
// was exactly one server process, but MCP clients do start a second instance —
// sometimes alongside the first rather than in place of it. The new process
// then came up blank: agent_list_tasks returned nothing and every task_id from
// before was unknown, while the workers those tasks owned kept running happily
// under the original process, still writing their results to disk. Nothing was
// broken except the server's ability to see its own work.
//
// This package closes that gap from both ends. It writes each task to disk as
// it progresses, so a later instance can list and read what came before. And it
// keeps a PID lock, so an instance can tell that it is not the only one running
// instead of silently presenting an empty registry as the truth.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is the on-disk home for task records and the instance lock.
// Its methods are safe for concurrent use.
type Store struct {
	dir string

	mu   sync.Mutex
	logs map[string]*os.File // append handles, one per task
}

// Owner identifies the process that holds the lock.
type Owner struct {
	PID     int       `json:"pid"`
	Started time.Time `json:"started"`
	Exe     string    `json:"exe"`
}

const (
	tasksSubdir = "tasks"
	lockFile    = "server.lock"
)

// DefaultDir is where state goes when the operator names no directory:
// %AppData%\cli-agent-mcp on Windows, ~/.config/cli-agent-mcp elsewhere.
func DefaultDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "cli-agent-mcp")
	}
	return filepath.Join(base, "cli-agent-mcp")
}

// Open prepares dir for use, creating it if needed.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("state dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Join(abs, tasksSubdir), 0o700); err != nil {
		return nil, fmt.Errorf("state dir %q: %w", abs, err)
	}
	return &Store{dir: abs, logs: map[string]*os.File{}}, nil
}

// Dir reports the directory in use, for logging and diagnostics.
func (s *Store) Dir() string { return s.dir }

// safeID rejects anything that could escape the tasks directory. Task ids are
// generated, not user-supplied, but a store that turns an id into a path should
// not depend on that staying true.
func safeID(id string) error {
	if id == "" {
		return fmt.Errorf("empty task id")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("task id %q has an unusable character %q", id, r)
		}
	}
	if strings.HasPrefix(id, ".") {
		return fmt.Errorf("task id %q may not start with a dot", id)
	}
	return nil
}

func (s *Store) taskPath(id, ext string) string {
	return filepath.Join(s.dir, tasksSubdir, id+ext)
}

// SaveTask writes v as the task's record, replacing any previous one.
//
// The write goes to a temp file and is renamed into place, so a process that
// dies mid-write leaves the previous record intact rather than a truncated one
// that would fail to parse on the next startup.
func (s *Store) SaveTask(id string, v any) error {
	if err := safeID(id); err != nil {
		return err
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode task %s: %w", id, err)
	}
	final := s.taskPath(id, ".json")
	tmp, err := os.CreateTemp(filepath.Dir(final), id+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, final); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// LoadTasks returns every stored record, oldest file first. Records that fail
// to parse are skipped rather than failing the whole load: one corrupt file
// must not cost the operator the rest of their history.
func (s *Store) LoadTasks() ([]json.RawMessage, error) {
	paths, err := filepath.Glob(filepath.Join(s.dir, tasksSubdir, "*.json"))
	if err != nil {
		return nil, err
	}
	sortByModTime(paths)

	out := make([]json.RawMessage, 0, len(paths))
	for _, p := range paths {
		buf, err := os.ReadFile(p)
		if err != nil || !json.Valid(buf) {
			continue
		}
		out = append(out, json.RawMessage(buf))
	}
	return out, nil
}

// LoadTask returns one stored record, or nil when there is none. It is how a
// task owned by another process gets re-read: that process keeps rewriting the
// record, so this is the only way to learn that it finished.
func (s *Store) LoadTask(id string) (json.RawMessage, error) {
	if err := safeID(id); err != nil {
		return nil, err
	}
	buf, err := os.ReadFile(s.taskPath(id, ".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !json.Valid(buf) {
		return nil, nil
	}
	return json.RawMessage(buf), nil
}

// AppendLine adds one transcript line to the task's log. Lines are written as
// they arrive so another instance can read a run that is still in progress.
func (s *Store) AppendLine(id, line string) error {
	if err := safeID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	f, ok := s.logs[id]
	if !ok {
		var err error
		f, err = os.OpenFile(s.taskPath(id, ".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		s.logs[id] = f
	}
	// A transcript line may itself contain newlines (a tool result, a stack
	// trace); escaping keeps one stored line equal to one transcript line, which
	// is what ReadLines and the in-memory line index both assume.
	_, err := fmt.Fprintln(f, escape(line))
	return err
}

func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "\n", `\n`)
}

// unescape reverses escape in a single pass, so a literal backslash-n in the
// agent's own output is not mistaken for a newline.
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// ReadLines returns a task's stored transcript.
func (s *Store) ReadLines(id string) ([]string, error) {
	if err := safeID(id); err != nil {
		return nil, err
	}
	buf, err := os.ReadFile(s.taskPath(id, ".log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	text := strings.TrimSuffix(strings.ReplaceAll(string(buf), "\r\n", "\n"), "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = unescape(l)
	}
	return lines, nil
}

// Forget drops a task's files, for when the manager evicts it from memory.
func (s *Store) Forget(id string) {
	if safeID(id) != nil {
		return
	}
	s.mu.Lock()
	if f, ok := s.logs[id]; ok {
		f.Close()
		delete(s.logs, id)
	}
	s.mu.Unlock()
	os.Remove(s.taskPath(id, ".json"))
	os.Remove(s.taskPath(id, ".log"))
}

// Prune keeps the newest `keep` task records and deletes the rest. Without it
// the directory would grow for the life of the installation.
func (s *Store) Prune(keep int) {
	if keep <= 0 {
		return
	}
	paths, err := filepath.Glob(filepath.Join(s.dir, tasksSubdir, "*.json"))
	if err != nil || len(paths) <= keep {
		return
	}
	sortByModTime(paths)
	for _, p := range paths[:len(paths)-keep] {
		// Via Forget rather than os.Remove: an open append handle makes the
		// transcript undeletable on Windows, which would leave the log behind
		// after its record was gone.
		s.Forget(strings.TrimSuffix(filepath.Base(p), ".json"))
	}
}

func sortByModTime(paths []string) {
	mod := make(map[string]time.Time, len(paths))
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			mod[p] = fi.ModTime()
		}
	}
	sort.Slice(paths, func(i, j int) bool { return mod[paths[i]].Before(mod[paths[j]]) })
}

// Acquire records this process as the owner of the state directory and reports
// the previous owner when that process is still alive.
//
// It deliberately does not refuse to start on a conflict. The client that just
// launched this process is talking to it and nothing else; failing here would
// leave the user with no server at all, which is worse than having two. The
// caller's job is to surface the conflict, not to prevent it.
func (s *Store) Acquire() (previous *Owner, err error) {
	path := filepath.Join(s.dir, lockFile)

	if buf, readErr := os.ReadFile(path); readErr == nil {
		var prev Owner
		if json.Unmarshal(buf, &prev) == nil && prev.PID != os.Getpid() && processAlive(prev.PID) {
			previous = &prev
		}
	}

	exe, _ := os.Executable()
	buf, err := json.Marshal(Owner{PID: os.Getpid(), Started: time.Now(), Exe: exe})
	if err != nil {
		return previous, err
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return previous, fmt.Errorf("write %s: %w", path, err)
	}
	return previous, nil
}

// Close releases the open log handles. It leaves the lock file in place: a
// stale lock is harmless because Acquire checks whether the recorded process is
// still alive, whereas deleting it on the way out would erase the evidence in
// exactly the case that matters — a process that was killed rather than shut
// down cleanly.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, f := range s.logs {
		f.Close()
		delete(s.logs, id)
	}
}
