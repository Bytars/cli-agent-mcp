// SPDX-License-Identifier: Apache-2.0

// Command pairing-e2e reproduces the six failure modes of issue #29 against the
// real built server, each with the control that proves the check can fail.
//
// # Why it exists
//
// Issue #29 was closed on six acceptance criteria that lived in a comment. A
// criterion in a comment is a promise: nobody can re-run it, and the day one of
// them silently stops holding is the day someone loses another working day to a
// server that will not serve. This is those six criteria as something a person
// or CI can run and get a verdict from.
//
// # The technique, because it is not obvious
//
// Pairing decides on WHO LAUNCHED the server. Nothing meaningful can therefore
// be tested by running the binary from a shell — the parent is the shell, every
// time. So the harness builds a tiny stand-in launcher (./launcher), copies it
// to several telling paths, and has IT run the server as a child; ParentExe()
// then reports the copy. MSIX identity needs no installed package: it is read
// entirely off the shape of the path, so a directory segment spelled
// Claude_1.40609.0.0_x64__pzs8sxrjxfjjc is a package family as far as the
// server is concerned.
//
// Most modes then have to backdate the record (see age): a fresh one adopts
// every launcher it sees for 24 hours, and on a record minutes old the
// welcoming and the refusing case look identical.
//
// # Every check keeps its control
//
// That is the house rule and it is load-bearing here in particular. Almost
// every assertion is a pattern matched against a message; a pattern with no
// counter-case is satisfied by a server that says the right words for the wrong
// reason, or by one that says them unconditionally. The controls are the paired
// runs that must come out the other way.
//
// Usage:
//
//	go run ./cmd/pairing-e2e                      # all six
//	PAIRING_E2E_ONLY=rescue go run ./cmd/pairing-e2e
//
// Exit status: 0 all passed, 1 something failed, 2 it could not run here.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

func main() {
	// Runs on Windows only, and says so rather than passing.
	//
	// It COMPILES everywhere, which is the part that matters for CI: the Linux
	// leg builds this package, so a change that breaks it is caught on both
	// legs. What it will not do is report a pass it did not earn. Every one of
	// these six failure modes is Windows-shaped — MSIX virtualizing %APPDATA%,
	// WindowsApps package paths, a client that rewrites its own install path
	// when it updates — and the hand-run proof this is a port of was made on
	// Windows. Several of the mechanisms underneath look portable and this gate
	// may well be liftable, but "looks portable" is not a thing to put a green
	// tick on; that is the exact currency issue #29 was paid in. Exit 2, the way
	// scripts/e2e.ps1 reports a skipped leg, so no caller mistakes it for 0.
	if runtime.GOOS != "windows" {
		fmt.Printf("SKIPPED: pairing-e2e drives real MSIX-shaped launcher paths and the %%APPDATA%% mirror, "+
			"so it only means anything on Windows; this is %s.\n"+
			"It still builds here on purpose, so a change that breaks it fails the Linux leg too.\n", runtime.GOOS)
		os.Exit(2)
	}

	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pairing-e2e:", err)
		os.Exit(1)
	}

	only := strings.TrimSpace(os.Getenv("PAIRING_E2E_ONLY"))
	if only != "" {
		if _, ok := modes[only]; !ok {
			fmt.Fprintf(os.Stderr, "pairing-e2e: no mode %q; PAIRING_E2E_ONLY accepts: %s\n", only, modeNames())
			os.Exit(1)
		}
	}

	fmt.Println("building the server and the stand-in launcher from", root)
	r, err := newRig(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pairing-e2e:", err)
		os.Exit(1)
	}
	defer r.cleanup()

	ran := 0
	for _, name := range ordered() {
		if only != "" && name != only {
			continue
		}
		m := modes[name]
		fmt.Printf("\n== #29 point %d — %s ==\n%s\n", m.Point, name, m.What)
		m.Run(r)
		ran++
	}

	fmt.Printf("\n  RESULT: %d passed, %d failed, across %d of %d modes\n", r.passed, r.failed, ran, len(modes))
	if r.failed > 0 {
		fmt.Printf("  failed: %s\n", strings.Join(r.failures, "; "))
		os.Exit(1)
	}
	// A run that covered only some of the modes is not the release gate either,
	// but it is what PAIRING_E2E_ONLY was asked for, so it is said rather than
	// scored.
	if ran < len(modes) {
		fmt.Printf("  note: PAIRING_E2E_ONLY=%s, so %d mode(s) were not run\n", only, len(modes)-ran)
	}
}
