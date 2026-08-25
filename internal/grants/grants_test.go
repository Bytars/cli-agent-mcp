// SPDX-License-Identifier: Apache-2.0

package grants

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGrantSurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Allows("PowerShell", "docker compose up -d") {
		t.Fatal("nothing was granted yet")
	}
	if err := s.Add(Grant{Tool: "PowerShell", Command: "docker"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// A permission given once must not have to be given again — that is the
	// entire reason for remembering it.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reopened.Allows("PowerShell", "docker compose up -d --build") {
		t.Error("the grant did not survive a restart")
	}
	if reopened.Allows("Bash", "docker ps") {
		t.Error("a grant for one tool must not cover another")
	}
	if reopened.Allows("PowerShell", "Remove-Item -Recurse") {
		t.Error("a grant for docker must not cover a different command")
	}
}

// The grant has to reach the agent as a pre-approval, or the worker still stops
// to ask and the remembering bought nothing.
func TestPatternsAreWhatTheAgentUnderstands(t *testing.T) {
	s, _ := Open(t.TempDir())
	_ = s.Add(Grant{Tool: "PowerShell", Command: "docker"})
	_ = s.Add(Grant{Tool: "WebFetch"})

	got := map[string]bool{}
	for _, p := range s.Patterns() {
		got[p] = true
	}
	if !got["PowerShell(docker:*)"] {
		t.Errorf("patterns = %v, want a command-scoped pattern", s.Patterns())
	}
	if !got["WebFetch"] {
		t.Errorf("patterns = %v, want the bare tool name for a whole-tool grant", s.Patterns())
	}
}

// Granting a whole tool makes the narrower grants under it meaningless; leaving
// them in the file implies a restriction that is no longer there.
func TestWholeToolGrantSupersedesCommandGrants(t *testing.T) {
	s, _ := Open(t.TempDir())
	_ = s.Add(Grant{Tool: "Bash", Command: "git"})
	_ = s.Add(Grant{Tool: "Bash", Command: "npm"})
	_ = s.Add(Grant{Tool: "Bash"})

	list := s.List()
	if len(list) != 1 || list[0].Command != "" {
		t.Errorf("grants = %+v, want only the whole-tool grant", list)
	}
	if !s.Allows("Bash", "anything at all") {
		t.Error("a whole-tool grant must cover every command")
	}
}

func TestRevokeSaysWhetherAnythingMatched(t *testing.T) {
	s, _ := Open(t.TempDir())
	_ = s.Add(Grant{Tool: "PowerShell", Command: "docker"})

	if removed, _ := s.Remove("PowerShell", "npm"); removed {
		t.Error("removing something that was never granted reported success")
	}
	removed, err := s.Remove("PowerShell", "docker")
	if err != nil || !removed {
		t.Fatalf("Remove: removed=%v err=%v", removed, err)
	}
	if s.Allows("PowerShell", "docker ps") {
		t.Error("the grant still applies after being revoked")
	}
}

func TestCommandVerbIgnoresEnvironmentPrefixes(t *testing.T) {
	cases := map[string]string{
		"docker compose up -d":      "docker",
		"NODE_ENV=production npm i": "npm",
		`"C:\Program Files\x.exe"`:  `C:\Program Files\x.exe`,
		"":                          "",
	}
	for in, want := range cases {
		if got := CommandVerb(in); got != want {
			t.Errorf("CommandVerb(%q) = %q, want %q", in, got, want)
		}
	}
}

// A grants file that will not parse must be reported, not silently treated as
// "nothing was ever granted" — that would re-ask for everything with no
// explanation anywhere.
func TestUnreadableStoreIsAnErrorNotAnEmptyList(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "grants.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Error("a corrupt grants file was accepted as an empty list")
	}
}
