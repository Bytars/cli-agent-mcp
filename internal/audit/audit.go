// SPDX-License-Identifier: Apache-2.0

// Package audit writes a structured, append-only JSONL trail of what the server
// asked the worker agents to do: which tasks started, the exact command lines
// executed, the tools the worker invoked, and how each turn ended.
//
// It exists because a headless worker can touch real infrastructure with no
// human watching each step; the audit log is the after-the-fact record of what
// actually ran.
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger appends JSONL audit records to a file. The zero value and a nil
// *Logger are both valid and disabled, so callers never need nil checks.
type Logger struct {
	mu sync.Mutex
	f  *os.File
}

// New opens (creating/appending) the audit log at path, creating the parent
// directory if needed. An empty path returns a disabled logger that discards
// everything.
func New(path string) (*Logger, error) {
	if path == "" {
		return &Logger{}, nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f}, nil
}

// Enabled reports whether records are actually written.
func (l *Logger) Enabled() bool {
	return l != nil && l.f != nil
}

// Log writes one record: {"ts","event", ...fields}. It is safe for concurrent
// use and never panics on a disabled logger.
func (l *Logger) Log(event string, fields map[string]any) {
	if !l.Enabled() {
		return
	}
	rec := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		rec[k] = v
	}
	rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	rec["event"] = event

	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(append(b, '\n'))
}

// Close closes the underlying file.
func (l *Logger) Close() error {
	if !l.Enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
