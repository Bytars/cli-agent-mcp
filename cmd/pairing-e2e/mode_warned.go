// SPDX-License-Identifier: Apache-2.0

package main

func init() {
	register("warned", mode{
		Point: 5,
		What: "Nobody is locked out without being told first. A launcher about to be refused gets one start\n" +
			"that says so — served, loudly — and only the start after that is refused.",
		Run: runWarned,
	})
}

// runWarned proves the announcement, and it proves it in the only way that
// counts: by showing that the warning is followed by the refusal it promised.
//
// Six lockouts over two days were diagnosed as crashes, bad paths and a missing
// binary before anyone suspected pairing, because from inside a client a
// refusal with nothing before it is indistinguishable from broken software.
func runWarned(r *rig) {
	state := r.freshState("warned")
	r.startAll(state, r.claudeV0)
	if err := age(state); err != nil {
		r.check("record could be aged", "^$", err.Error())
		return
	}

	// The two checks are each other's control, which is why they must stay
	// together and in this order.
	//
	// Alone, the first is satisfied by a server that warns for ever and never
	// refuses — the announcement would be theatre and the exposure it concedes
	// would be permanent. Alone, the second is satisfied by a server that
	// refuses immediately, which is the lockout this whole mode exists to
	// prevent. Only the pair pins the behaviour to exactly one free start.
	r.check("1st encounter with another program -> warns and serves",
		"^SERVING THIS ONCE", r.startLine(state, r.editor))
	r.check("2nd encounter -> now it refuses",
		"^refusing", r.startLine(state, r.editor))
}
