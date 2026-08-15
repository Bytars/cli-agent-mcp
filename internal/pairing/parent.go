// SPDX-License-Identifier: Apache-2.0

package pairing

// ParentExe reports the absolute path of the executable that launched this
// process, or "" when the platform cannot answer.
//
// # Why bind to the launcher at all
//
// A token alone is only as private as the file it is configured in, and that
// file is readable by anything running as the user. Binding adds a second fact
// the attacker has to satisfy — "you must be started by that exact program" —
// which a secret copied out of a config file does not give them.
//
// # What binding does not stop
//
// On Windows a process able to open the real client with PROCESS_CREATE_PROCESS
// can declare it as the parent of something it spawns, so a determined
// same-user attacker can present the right lineage. Verifying the parent's code
// signature would not help there either: the spoofed parent *is* the genuine,
// correctly signed client. That is why this records a path rather than an
// Authenticode or Team ID chain — the extra machinery would buy nothing against
// the attack that actually applies, and the path is what an attacker who cannot
// write to the client's install directory genuinely cannot fake.
//
// Treat it as the layer that makes a leaked token much less useful, not as a
// boundary that survives an attacker already running code as the user.
func ParentExe() string {
	exe, err := parentExe()
	if err != nil || exe == "" {
		return ""
	}
	return exe
}
