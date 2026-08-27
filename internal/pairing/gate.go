// SPDX-License-Identifier: Apache-2.0

package pairing

import "fmt"

// Explain renders a rejection for the two audiences that need it.
//
// The stderr line is for the person reading a log; the client line is for the
// model on the other end of the pipe, which is the only party that will ever
// see a tool result. Getting the second one right matters more than it looks:
// the alternative — exiting non-zero on a bad token — surfaces in Claude
// Desktop as nothing but "server disconnected", and the user has no way to tell
// a pairing problem from a crash, a bad path, or a missing binary.
func Explain(r Result) (stderr, client string) {
	const fix = "Run `cli-agent-mcp pair --install` in a terminal, then restart the client."

	switch r.Status {
	case NoToken:
		return "refusing to serve: " + r.Detail,
			"This cli-agent-mcp server is paired to specific clients, and whatever launched it presented no credential, " +
				"so every tool here is disabled. Tell the user, verbatim: " + fix + " " +
				"Do not try to work around this and do not retry — nothing will change until they do that."

	case BadToken:
		return "refusing to serve: " + r.Detail,
			"This cli-agent-mcp server rejected the credential it was launched with, so every tool here is disabled. " +
				"Either the token in the client's config is stale (it was rotated or revoked) or something other than " +
				"the paired client started this server. Tell the user, verbatim: " + fix + " " +
				"Do not retry — the answer will be the same."

	case ForeignParent:
		return "refusing to serve: " + r.Detail,
			"This cli-agent-mcp server holds a valid credential but was started by a program it is not bound to, " +
				"so every tool here is disabled. This is what it looks like when a token has been reused by something " +
				"other than the client it was issued for — worth mentioning to the user plainly. If they moved or " +
				"reinstalled that client themselves, the fix is: `cli-agent-mcp pair --unbind " + r.Label + "`, then restart the client."
	}
	return "", ""
}

// StartupLine is what the server logs about its own pairing state.
func StartupLine(stateDir string, r Result) string {
	switch r.Status {
	case OK:
		return fmt.Sprintf("paired: authorized as %q", r.Label)
	case Unpaired:
		return "NOT PAIRED: any process on this machine can start this server and delegate work to an agent " +
			"that inherits your environment. Run `cli-agent-mcp pair --install` to close that."
	}
	f, _, err := Load(stateDir)
	if err != nil {
		return "locked"
	}
	return fmt.Sprintf("locked (issued tokens: %s)", Labels(f))
}

// StatusName renders a status for the audit trail.
func StatusName(s Status) string {
	switch s {
	case Unpaired:
		return "unpaired"
	case OK:
		return "ok"
	case NoToken:
		return "no_token"
	case BadToken:
		return "bad_token"
	case ForeignParent:
		return "foreign_parent"
	}
	return "unknown"
}
