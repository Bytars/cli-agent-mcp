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
// cmd renders a rescue command with this verdict's state directory already in
// it.
//
// Every instruction in this file used to be written as `cli-agent-mcp trust
// --add`, leaving the reader to run it from a terminal — where it resolves its
// own directory, and under MSIX resolves a different one than the server. Four
// rescue attempts in a row did nothing on 1-sep for exactly that reason, and
// nobody could see why: both sides reported success about different files.
//
// So the command is quoted whole, with --state-dir filled in from the verdict.
// It is longer to read and it is the difference between an instruction that
// works and one that looks like it worked (issue #29).
func cmd(r Result, args string) string {
	if r.StateDir == "" {
		return "`cli-agent-mcp " + args + "`"
	}
	return "`cli-agent-mcp " + args + " --state-dir \"" + r.StateDir + "\"`"
}

// exactamente warns the model not to tidy the command — but only when there is
// a --state-dir to preserve.
//
// Mentioning the flag unconditionally is worse than saying nothing: with no
// directory to fill in there is no flag in the command, and telling someone to
// keep a flag that is not there invites them to add an empty one, which is a
// syntax error. Caught by this file's own control test.
func exactamente(r Result) string {
	if r.StateDir == "" {
		return ""
	}
	return "Give them the command exactly as written, --state-dir included: without it the command " +
		"resolves a different directory than this server reads, and appears to succeed while changing nothing. "
}

func Explain(r Result) (stderr, client string) {
	fix := "Run " + cmd(r, "pair --install") + " in a terminal, then restart the client."

	// The way out when pairing was set up against the wrong place. It has to be
	// offered here and not only in the docs: on this path the user has no
	// working MCP, so the answer cannot be "go read something" (issue #25).
	escape := "If pairing was just set up and the client stopped working, the token is not reaching this " +
		"server — run " + cmd(r, "pair --unpair") + " to get the client running again, then put the token " +
		"where the program that launches this server will actually read it."

	switch r.Status {
	case NoToken:
		// Name the launcher. This process knows who started it, and that is
		// exactly the fact the user needs: the token belongs in THAT program's
		// configuration, which may not be the file the installer wrote.
		// Same hedge as the detail in Verify, and for the same reason: the parent
		// can be a shim or a shell standing between the client and this process,
		// so "that is where the token goes" overstates what was observed. This
		// message is the one the model repeats out loud, which makes it the worse
		// place of the two to overstate anything.
		who := "The program that launched it"
		if r.Launcher != "" {
			who = "It was launched by " + r.Launcher + ", so the token has to reach that program — or whatever it passes its environment down to; it"
		}
		// The escape goes in the log too, not only in the message for the model.
		// In this state the user's client is broken, so the model's relay may
		// never reach them; the log is the one channel they certainly have, and
		// where this gets diagnosed. A way out only in the message they cannot
		// read repeats the mistake this exists to fix.
		return "refusing to serve: " + r.Detail + "\n" + escape,
			"This cli-agent-mcp server is paired to specific clients, and whatever launched it presented no credential, " +
				"so every tool here is disabled. " + who + " presented no CLI_AGENT_MCP_TOKEN. " +
				"Tell the user, verbatim: " + fix + " " + escape + " " +
				"Do not try to work around this and do not retry — nothing will change until they do that."

	case BadToken:
		return "refusing to serve: " + r.Detail,
			"This cli-agent-mcp server rejected the credential it was launched with, so every tool here is disabled. " +
				"Either the token in the client's config is stale (it was rotated or revoked) or something other than " +
				"the paired client started this server. Tell the user, verbatim: " + fix + " " +
				"Do not retry — the answer will be the same."

	case Armed:
		// Not a refusal, so it does not tell the model to stop. It says what is
		// true — the door is not shut yet — and how to shut it. A trial nobody
		// notices is just a weaker default.
		return "PAIRING NOT YET IN EFFECT: " + r.Detail + ".\n" +
				"Restart the client that should be driving this server; the first launch that presents the " +
				"token turns enforcement on for good. Until then any local process can still use this server. " +
				"To skip the wait, run `cli-agent-mcp pair --enforce-now`.",
			"This cli-agent-mcp server has been paired, but no launcher has presented the token yet, so it is " +
				"still serving anyone — including this session. Tell the user plainly: pairing is configured and " +
				"NOT yet in effect, and it starts working the first time the client they configured launches this " +
				"server with the token. If that never happens, the token did not reach that client's configuration."

	case ForeignLauncher:
		// The recovery here is one command with no secret in it, which is the
		// whole point of the launcher mode: being locked out is a nuisance
		// rather than an emergency. Say the command, and say it in the log too —
		// the person reading it has no working client.
		// The commands go in the LOG too, with the directory filled in. The
		// Detail comes from Verify, which words the rejection but cannot quote a
		// command — and the log is what the locked-out person actually reads.
		salidas := "To fix: " + cmd(r, "trust --add") + " run from that client, or " +
			cmd(r, "trust --reset") + " to forget the old one."
		return "refusing to serve: " + r.Detail + "\n" + salidas,
			"This cli-agent-mcp server only answers to the program that set it up, and something else started it, " +
				"so every tool here is disabled. That is what it looks like when another program on this machine tries " +
				"to use the server — worth telling the user plainly. If instead they just moved or updated their client, " +
				"the fix is " + cmd(r, "trust --add") + " run from that client, or " + cmd(r, "trust --reset") + " to forget " +
				"the old one. " + exactamente(r) + "Do not retry — the answer will be the same."

	case EmptyRecord:
		// Name BOTH ways out. An emptied launcher list and a revoked last token
		// leave the same record behind, so this message cannot know which
		// mechanism the user was on — and guessing "token" is how somebody who
		// never held one gets told to go put a secret somewhere.
		bothWaysOut := "Run " + cmd(r, "trust --reset") + " to go back to trusting whichever program starts " +
			"this server, or " + cmd(r, "pair --install") + " to authorize with a token instead."
		return "refusing to serve: " + r.Detail + "\n" + bothWaysOut,
			"This cli-agent-mcp server has a pairing record with nothing in it — no token and no trusted launcher — " +
				"so nothing can authenticate and every tool here is disabled. That is what a revoked last token or a " +
				"removed last launcher leaves behind. Tell the user, verbatim: " + bothWaysOut + " " +
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
//
// It always names the directory. Under MSIX the server and a terminal see
// different values for %APPDATA%, so "which record am I actually reading" is
// not something anyone can work out from the outside — and not printing it is
// what made a whole day of rescue attempts unfalsifiable (issue #29).
func StartupLine(stateDir string, r Result) string {
	return startupVerdict(stateDir, r) + " [record: " + Path(stateDir) + "]"
}

func startupVerdict(stateDir string, r Result) string {
	switch r.Status {
	case OK:
		return fmt.Sprintf("paired: authorized as %q", r.Label)
	case Unpaired:
		return "NOT PAIRED: any process on this machine can start this server and delegate work to an agent " +
			"that inherits your environment. Run `cli-agent-mcp pair --install` to close that."
	case Armed:
		return "armed, NOT enforcing: paired, but no launcher has presented the token yet"
	case TrustedLauncher:
		if r.Launcher == "" {
			return "trusting the launching program (this platform cannot name it, so the check was skipped)"
		}
		return "authorized: started by " + r.Launcher
	case ForeignLauncher:
		return "refusing to serve: " + r.Detail
	case EmptyRecord:
		return "locked: the record holds neither a token nor a trusted launcher"
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
	case Armed:
		return "armed"
	case TrustedLauncher:
		return "trusted_launcher"
	case ForeignLauncher:
		return "foreign_launcher"
	case EmptyRecord:
		return "empty_record"
	}
	return "unknown"
}
