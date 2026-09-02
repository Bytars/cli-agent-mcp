// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"sort"
	"strings"
)

// A mode is one of the six failure modes of issue #29, proved end to end.
//
// Modes live in files of their own and register themselves, for the same reason
// smoketest's scenarios do: each is a self-contained argument about one
// behaviour and reads better whole than as another branch in main. The shape is
// copied from cmd/smoketest deliberately — someone who has read one harness in
// this repo should not have to learn a second set of conventions.
type mode struct {
	// Point is which numbered point of issue #29 this covers. It orders the run
	// and it is printed, so a report can be laid next to the issue and read off
	// line by line, which is the whole reason this program exists.
	Point int

	// What it proves, printed before it runs so a failing log says what was
	// being asserted, not just where it stopped.
	What string

	Run func(r *rig)
}

var modes = map[string]mode{}

// register wires a mode in. Called from each file's init.
func register(name string, m mode) {
	if _, dup := modes[name]; dup {
		panic("pairing-e2e: duplicate mode " + name)
	}
	modes[name] = m
}

// ordered lists the registered modes by issue point, which is the order a
// person reading issue #29 expects them in.
func ordered() []string {
	names := make([]string, 0, len(modes))
	for n := range modes {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if a, b := modes[names[i]].Point, modes[names[j]].Point; a != b {
			return a < b
		}
		return names[i] < names[j]
	})
	return names
}

// modeNames lists what PAIRING_E2E_ONLY accepts, for the error when it is given
// something else.
func modeNames() string {
	return strings.Join(ordered(), ", ")
}

// check records one assertion and its outcome.
//
// EVERY CALL SITE OWES A CONTROL, and that is not a style preference. Nearly all
// of these read a message out of the server, and a pattern that matches
// whatever the server happens to say is indistinguishable from a pattern that
// matches everything. The control is the paired call whose expected verdict is
// the opposite one: it is what makes a green run evidence rather than a wish.
//
// The failing branch prints the pattern AND the full text that missed it,
// because the interesting failures here are ones where the server said
// something sensible that is not what was asserted, and a bare "FAIL" hides
// exactly that.
func (r *rig) check(name, pattern, got string) {
	if matches(pattern, got) {
		r.passed++
		fmt.Printf("    PASS  %s\n", name)
		return
	}
	r.failed++
	r.failures = append(r.failures, name)
	fmt.Printf("    FAIL  %s\n          expected /%s/\n          got      %s\n", name, pattern, oneLine(got))
}

// oneLine folds a multi-line capture so a failure stays one readable block. The
// startup output is several lines and a raw dump of it buries the assertion
// that tripped.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
