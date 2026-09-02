// SPDX-License-Identifier: Apache-2.0

package main

func init() {
	register("update", mode{
		Point: 1,
		What: "A background client update must not lock the user out. Claude Desktop's path carries its\n" +
			"version number, so an update rewrites it; the binding has to survive that and still refuse a\n" +
			"genuinely different package.",
		Run: runUpdate,
	})
}

// runUpdate is the failure that started issue #29: the app updated itself
// overnight from 1.40609.0.0 to 1.40609.1.0, the recorded path stopped
// matching, and every tool in the server disappeared with no explanation.
func runUpdate(r *rig) {
	state := r.freshState("update")

	// One start to get the launcher recorded, then close the learning window.
	// Skipping the backdating would make both checks below pass for a reason
	// that has nothing to do with identity: inside the window a fresh record
	// adopts anything, including the control.
	r.startAll(state, r.claudeV0)
	if err := age(state); err != nil {
		r.check("record could be aged", "^$", err.Error())
		return
	}

	r.check("same app, new version -> served",
		"^authorized", r.startLine(state, r.claudeV1))

	// THE CONTROL. Same executable name, same WindowsApps layout, same version
	// shape — only the package family differs. Without it, "authorized" above
	// is equally consistent with a check that matches on `claude.exe` and would
	// therefore wave through anything that named itself well.
	r.check("CONTROL: a different package is not authorized",
		"SERVING THIS ONCE|refusing", r.startLine(state, r.other))
}
