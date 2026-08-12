// Package grants remembers permissions the user has already given.
//
// Answering the same question twice is the fastest way to make an approval
// flow useless: if every run asks again whether the worker may run docker, the
// person stops reading the questions and starts saying yes to all of them.
//
// A remembered grant does two things. It answers a future request for the same
// thing without asking, and it is fed to the agent as a pre-approved tool on
// the next run — so the worker never even reaches the point of asking. The
// second is the one that matters: an approval that has to travel back through
// a conversation costs seconds and attention, and the whole point of granting
// it permanently is not to spend either again.
//
// Grants are deliberately coarse and readable. "PowerShell running docker" is
// something a person can hold in their head and revoke later; a hash of an
// exact command line is not.
package grants

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

// Grant is one remembered permission.
type Grant struct {
	// Tool is the agent's tool name, e.g. "Bash", "PowerShell", "WebFetch".
	Tool string `json:"tool"`

	// Command is the first word of a shell command — "docker", "npm", "git" —
	// or empty when the grant covers the whole tool. Keeping it to the verb is
	// what makes a grant something a person can still reason about a week later.
	Command string `json:"command,omitempty"`

	GrantedAt time.Time `json:"granted_at"`
	Note      string    `json:"note,omitempty"`
}

// Pattern renders the grant the way the agent's allow-list expects it.
func (g Grant) Pattern() string {
	if g.Command == "" {
		return g.Tool
	}
	return fmt.Sprintf("%s(%s:*)", g.Tool, g.Command)
}

// String renders the grant for a person.
func (g Grant) String() string {
	if g.Command == "" {
		return g.Tool + " (any use)"
	}
	return g.Tool + " running " + g.Command
}

// Store holds the remembered grants for this machine.
type Store struct {
	path string

	mu   sync.Mutex
	list []Grant
}

// Open loads the store at dir, creating nothing until something is granted.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("grants: no directory")
	}
	s := &Store{path: filepath.Join(dir, "grants.json")}
	buf, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	// A grants file that will not parse must not silently become "no grants":
	// that would re-ask for everything with no explanation. Say so instead.
	if err := json.Unmarshal(buf, &s.list); err != nil {
		return nil, fmt.Errorf("grants: %s is not readable: %w", s.path, err)
	}
	return s, nil
}

// Path reports where grants are stored, for diagnostics.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// CommandVerb extracts the part of a shell command a grant is keyed on: the
// program being run, ignoring any environment prefix.
//
// It stays at the first word on purpose. "docker" is a decision someone can
// make; "docker compose -f ./x.yml up -d --build" is a decision nobody can
// meaningfully repeat, and remembering it would mean asking again the moment a
// flag changed.
func CommandVerb(command string) string {
	for _, field := range splitRespectingQuotes(command) {
		// Skip a leading VAR=value, which is a prefix rather than the program.
		if strings.Contains(field, "=") && !strings.HasPrefix(field, "-") {
			continue
		}
		return field
	}
	return ""
}

// splitRespectingQuotes splits on whitespace but keeps a quoted run together.
//
// Plain whitespace splitting turns `"C:\Program Files\app.exe" --flag` into a
// grant keyed on `C:\Program`, which matches nothing anyone would recognise and
// would silently pre-approve a path that does not exist. On Windows, where the
// interesting programs live under a directory with a space in its name, this is
// the common case rather than the exotic one.
func splitRespectingQuotes(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune

	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// Allows reports whether something already granted covers this request.
func (s *Store) Allows(tool, command string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	verb := CommandVerb(command)
	for _, g := range s.list {
		if !strings.EqualFold(g.Tool, tool) {
			continue
		}
		if g.Command == "" {
			return true // the whole tool was granted
		}
		if verb != "" && strings.EqualFold(g.Command, verb) {
			return true
		}
	}
	return false
}

// Add records a grant, replacing any narrower one for the same tool. It is a
// no-op when an existing grant already covers it.
func (s *Store) Add(g Grant) error {
	if s == nil {
		return fmt.Errorf("grants: no store")
	}
	if strings.TrimSpace(g.Tool) == "" {
		return fmt.Errorf("grants: a grant needs a tool name")
	}
	if g.GrantedAt.IsZero() {
		g.GrantedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, existing := range s.list {
		if strings.EqualFold(existing.Tool, g.Tool) &&
			(existing.Command == "" || strings.EqualFold(existing.Command, g.Command)) {
			return nil
		}
	}
	// Granting a whole tool supersedes the per-command grants under it, which
	// would otherwise sit in the file implying a narrowness that is gone.
	if g.Command == "" {
		kept := s.list[:0]
		for _, existing := range s.list {
			if !strings.EqualFold(existing.Tool, g.Tool) {
				kept = append(kept, existing)
			}
		}
		s.list = kept
	}
	s.list = append(s.list, g)
	return s.saveLocked()
}

// Remove drops a grant. It reports whether anything matched, because "revoked"
// and "there was nothing to revoke" must not read the same.
func (s *Store) Remove(tool, command string) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("grants: no store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := make([]Grant, 0, len(s.list))
	removed := false
	for _, g := range s.list {
		match := strings.EqualFold(g.Tool, tool) &&
			(command == "" || strings.EqualFold(g.Command, command))
		if match {
			removed = true
			continue
		}
		kept = append(kept, g)
	}
	if !removed {
		return false, nil
	}
	s.list = kept
	return true, s.saveLocked()
}

// List returns the grants, newest first.
func (s *Store) List() []Grant {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := append([]Grant(nil), s.list...)
	sort.Slice(out, func(i, j int) bool { return out[i].GrantedAt.After(out[j].GrantedAt) })
	return out
}

// Patterns renders every grant for the agent's pre-approval flag, so a
// permission given once is never asked for again.
func (s *Store) Patterns() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.list))
	for _, g := range s.list {
		out = append(out, g.Pattern())
	}
	return out
}

// saveLocked writes via a temp file and a rename, so a process that dies
// mid-write leaves the previous list intact rather than a truncated one. Caller
// holds mu.
func (s *Store) saveLocked() error {
	buf, err := json.MarshalIndent(s.list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "grants.*.tmp")
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
	if err := os.Rename(name, s.path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
