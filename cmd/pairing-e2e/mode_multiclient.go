// SPDX-License-Identifier: Apache-2.0

package main

func init() {
	register("multiclient", mode{
		Point: 6,
		What: "Two clients on one machine. A fresh record spends its first day adopting whichever programs\n" +
			"start the server, so someone with a client and an editor uses both without noticing pairing\n" +
			"exists — and the window has to actually close afterwards.",
		Run: runMultiClient,
	})
}

// runMultiClient covers the trade learningWindow makes. Recording the first
// launcher and refusing everyone after would brick a working two-client setup on
// upgrade, which is worse than the exposure it closes; a window that never
// closed would be a door that never shuts.
func runMultiClient(r *rig) {
	state := r.freshState("multiclient")

	r.check("client A gets in",
		"^authorized", r.startLine(state, r.claudeV0))
	// Not the same program, not the same kind of identity — the editor is an
	// ordinary path rather than an MSIX package — and still admitted, because
	// the record is hours old.
	r.check("client B gets in too (window still open)",
		"^authorized", r.startLine(state, r.editor))

	// THE CONTROL, and this one is the whole point of the mode. Both passes
	// above are exactly what a server with no check at all would produce. Aging
	// the record shuts the window; a third program arriving afterwards must not
	// be waved through the way the first two were.
	if err := age(state); err != nil {
		r.check("record could be aged", "^$", err.Error())
		return
	}
	r.check("CONTROL: a third program does not walk straight in",
		"SERVING THIS ONCE|refusing", r.startLine(state, r.other))
}
