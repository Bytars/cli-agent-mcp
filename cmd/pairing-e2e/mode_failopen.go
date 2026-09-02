// SPDX-License-Identifier: Apache-2.0

package main

import "os"

func init() {
	register("failopen", mode{
		Point: 4,
		What: "A record the server cannot parse must cost nobody their server — and must not be swallowed\n" +
			"quietly either, because whatever it described is no longer protecting anyone.",
		Run: runFailOpen,
	})
}

// runFailOpen is fail-open WITH NOISE. Both halves are the criterion: serving
// on an unreadable record is what stops a corrupted file from locking a user
// out, and saying so loudly is what stops the machine from sitting wide open
// while everyone believes pairing is on.
func runFailOpen(r *rig) {
	// Deliberate garbage, not a plausible-looking record: what is being measured
	// is the path where parsing fails outright.
	state := r.freshState("failopen")
	r.pair(state, "--label", "prueba")
	if err := os.WriteFile(recordPath(state), []byte("{ not json"), 0o600); err != nil {
		r.check("corrupt record could be written", "^$", err.Error())
		return
	}
	// startAll, not startLine: on this path the server logs the read error
	// FIRST, so the verdict is not the first line and the mode needs all of it.
	out := r.startAll(state, r.claudeV0)
	r.check("unreadable record -> still serves",
		"SERVING, pairing record ignored", out)
	r.check("unreadable record -> and shouts about it",
		"NO PAIRING IN EFFECT", out)
	// The third check is not a restatement of the second. The two lines come
	// from different places — the verdict from the gate, the diagnosis from the
	// read — and losing the diagnosis leaves the user told that pairing is off
	// with no way to find out why.
	r.check("unreadable record -> and does NOT go silent about the cause",
		"is corrupt", out)
	// AND the tools are actually reachable. Every check above reads what the
	// server SAYS, and that is not the criterion: with Result.Allowed() mutated
	// to drop UnreadableRecord, the server refuses every tool through the
	// lockdown middleware while all three lines above stay exactly the same.
	// This harness passed that mutation, which is how the hole was found.
	r.check("unreadable record -> and the tools really are reachable",
		"^served$", servedOrNot(callTool(r, state, "agent_list_agents")))

	// A UTF-8 BOM is the realistic version of the same fault: it is what a
	// well-meaning editor, or a PowerShell redirect, leaves on a file that is
	// otherwise perfectly valid JSON. If that locked people out, the lockout
	// would arrive from an entirely innocent act.
	state = r.freshState("failopen-bom")
	r.pair(state, "--label", "prueba")
	if err := prependBOM(recordPath(state)); err != nil {
		r.check("BOM could be prepended", "^$", err.Error())
		return
	}
	r.check("UTF-8 BOM -> serves just the same",
		"SERVING, pairing record ignored", r.startAll(state, r.claudeV0))

	// THE CONTROL. Everything above is equally consistent with a server that
	// stopped enforcing pairing altogether, which would be a far worse bug than
	// the one being fixed. An intact record must still rule.
	state = r.freshState("failopen-control")
	r.pair(state, "--label", "prueba")
	r.check("CONTROL: a readable record still rules",
		"PAIRING NOT YET IN EFFECT|refusing|armed", r.startLine(state, r.claudeV0))
}

// prependBOM puts a UTF-8 byte-order mark in front of an otherwise valid record.
func prependBOM(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte{0xEF, 0xBB, 0xBF}, b...), 0o600)
}
