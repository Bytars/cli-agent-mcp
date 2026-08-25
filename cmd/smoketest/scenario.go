// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A scenario is one end-to-end proof, selected with SMOKE_ONLY=<name>.
//
// Scenarios live in files of their own and register themselves, rather than
// being another branch in main. Two reasons. Each is a self-contained argument
// about one behaviour and reads better whole, and a scenario that needs a real
// agent cannot run in CI — keeping them separable is what lets the default path
// stay mock-friendly while the interesting ones are pointed at a real install.
type scenario struct {
	// Needs says which agent this scenario requires to mean anything. Empty
	// runs anywhere; "real" refuses the mock, because a scenario that silently
	// passes against a stub is worse than one that does not run.
	Needs string

	// What it proves, printed before it runs so a failing log says what was
	// being asserted, not just where it stopped.
	What string

	Run func(ctx context.Context, e *env)
}

// env is what a scenario needs to drive the server.
type env struct {
	Session *mcp.ClientSession
	Agent   string
	Cwd     string
	Prompt  string

	// Progress returns everything streamed so far, as a copy.
	Progress func() []string
}

var scenarios = map[string]scenario{}

// register wires a scenario in. Called from each file's init.
func register(name string, s scenario) {
	if _, dup := scenarios[name]; dup {
		panic("smoketest: duplicate scenario " + name)
	}
	scenarios[name] = s
}

// scenarioNames lists what SMOKE_ONLY accepts, for the error when it is given
// something else.
func scenarioNames() string {
	names := make([]string, 0, len(scenarios))
	for n := range scenarios {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
