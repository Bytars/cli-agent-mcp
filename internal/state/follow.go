package state

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// This file is the read-only half of the store: what a *second* process needs
// to watch work owned by the first one.
//
// Everything a task produces is already on disk while it is producing it —
// AppendLine writes each transcript line as it arrives, and persist rewrites the
// record at every transition. So a viewer needs no channel into the server, no
// port, and no cooperation from it: it just reads the same files, from an
// offset, as they grow. Nothing here writes, and that is deliberate — see
// Owner for the one case where writing would actively break the server.

const (
	// tailWindow bounds the backward scan that positions a follower N lines from
	// the end. Transcripts run to tens of thousands of lines; no one asks for a
	// tail that reaches back past a megabyte of them.
	tailWindow = 1 << 20

	// maxChunk bounds a single Next call, so attaching to a large backlog
	// arrives in a few bounded reads instead of one allocation the size of the
	// whole file.
	maxChunk = 4 << 20
)

// Follower streams a task's transcript as it grows, returning only what is new
// since the last call. It is the primitive behind `cli-agent-mcp logs -f` and
// the local web viewer.
//
// It re-opens the file on every poll rather than holding a handle. At this
// cadence that costs nothing, and it keeps a reader from being one more handle
// contending with the writer's own append handle on Windows.
type Follower struct {
	path string
	off  int64
	part []byte // bytes after the last newline: an incomplete line, still being written
}

// Follow opens a follower over a task's transcript.
//
// lastN >= 0 positions it so the first Next returns the final lastN lines
// already on disk (0 means "only what arrives from now on"); lastN < 0 starts
// at the beginning of the transcript. A task that has not written anything yet
// is not an error — the follower simply yields nothing until it does.
func (s *Store) Follow(id string, lastN int) (*Follower, error) {
	if err := safeID(id); err != nil {
		return nil, err
	}
	f := &Follower{path: s.taskPath(id, ".log")}
	if lastN < 0 {
		return f, nil
	}

	fh, err := os.Open(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	defer fh.Close()

	st, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if lastN == 0 {
		f.off = size
		return f, nil
	}

	start := size - tailWindow
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if _, err := fh.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, err
	}
	f.off = start + int64(backNLines(buf, lastN, start > 0))
	return f, nil
}

// backNLines returns the offset within buf where the last n complete lines
// begin. windowed says buf starts mid-file, in which case its first line is a
// fragment of an earlier one and must be skipped rather than emitted as if it
// were whole.
func backNLines(buf []byte, n int, windowed bool) int {
	end := len(buf)
	if end > 0 && buf[end-1] == '\n' {
		end-- // the final newline terminates the last line, it does not start one
	}
	count := 0
	for i := end; i > 0; i-- {
		if buf[i-1] == '\n' {
			count++
			if count == n {
				return i
			}
		}
	}
	if windowed {
		if nl := strings.IndexByte(string(buf), '\n'); nl >= 0 {
			return nl + 1
		}
		return len(buf)
	}
	return 0
}

// Next returns the transcript lines that appeared since the previous call, in
// order. It never blocks: no new output yields no lines, which is what lets a
// caller poll it on its own schedule.
func (f *Follower) Next() ([]string, error) {
	fh, err := os.Open(f.path)
	if err != nil {
		// A transcript that does not exist yet is the normal state of a task
		// that has just started, and of one whose record was pruned. Neither is
		// something a tail should fail on.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()

	st, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < f.off {
		// The file shrank, so it is not the file we were reading — a pruned id
		// whose store was rebuilt. Reading on from the old offset would splice
		// two transcripts together; start over instead.
		f.off, f.part = 0, nil
	}
	if st.Size() == f.off {
		return nil, nil
	}

	if _, err := fh.Seek(f.off, io.SeekStart); err != nil {
		return nil, err
	}
	n := st.Size() - f.off
	if n > maxChunk {
		n = maxChunk
	}
	buf := make([]byte, n)
	read, err := io.ReadFull(fh, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	f.off += int64(read)

	data := buf[:read]
	if len(f.part) > 0 {
		data = append(f.part, data...)
		f.part = nil
	}

	var out []string
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			out = append(out, unescape(strings.TrimSuffix(string(data[start:i]), "\r")))
			start = i + 1
		}
	}
	if start < len(data) {
		// Hold the tail back. The writer is mid-line; emitting it now would show
		// half an event and then repeat it whole on the next poll.
		f.part = append([]byte(nil), data[start:]...)
	}
	return out, nil
}

// Owner reports the process recorded in the lock file, when that process is
// still alive.
//
// It exists because Acquire cannot be used for this: Acquire *writes* the lock,
// so a read-only viewer calling it would take ownership away from the running
// server and leave the next real instance believing it was alone. Reading is the
// whole contract here.
func (s *Store) Owner() *Owner {
	buf, err := os.ReadFile(filepath.Join(s.dir, lockFile))
	if err != nil {
		return nil
	}
	var o Owner
	if json.Unmarshal(buf, &o) != nil || o.PID == 0 || !processAlive(o.PID) {
		return nil
	}
	return &o
}
