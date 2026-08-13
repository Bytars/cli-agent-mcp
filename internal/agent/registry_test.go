// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"context"
	"os/exec"
	"testing"
)

type namedAdapter struct{ name string }

func (n *namedAdapter) Name() string              { return n.name }
func (n *namedAdapter) Available() (bool, string) { return true, "stub" }
func (n *namedAdapter) ParseLine(string) Event    { return Event{} }
func (n *namedAdapter) Command(context.Context, RunSpec) (*exec.Cmd, error) {
	return nil, nil
}

func TestRegistryLookupIsCaseInsensitiveBothWays(t *testing.T) {
	// Registering "Aider" and looking it up used to silently fail: writes kept
	// the original case, reads lower-cased. The agent showed as available and
	// was unreachable.
	r := NewRegistry(&namedAdapter{name: "Aider"})
	for _, q := range []string{"Aider", "aider", "  AIDER  "} {
		if r.Get(q) == nil {
			t.Errorf("Get(%q) returned nil", q)
		}
	}
}

func TestRegistryKeepsFirstOnCollision(t *testing.T) {
	first := &namedAdapter{name: "claude"}
	second := &namedAdapter{name: "Claude"}
	r := NewRegistry(first, second)

	if got := r.Get("claude"); got != Adapter(first) {
		t.Error("a colliding name must not silently replace the earlier adapter")
	}
	if n := len(r.Names()); n != 1 {
		t.Errorf("expected one registered name, got %d", n)
	}
}

func TestRegistryIgnoresEmptyName(t *testing.T) {
	r := NewRegistry(&namedAdapter{name: "   "})
	if n := len(r.Names()); n != 0 {
		t.Errorf("an adapter with a blank name must be ignored, got %d entries", n)
	}
}
