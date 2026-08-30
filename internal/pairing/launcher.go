// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"time"
)

// Trusting the first launcher: authorization with no secret at all.
//
// # Why this exists next to the token
//
// The token was built to keep any local process from using this server as a
// confused deputy. It does that — but only as well as the secret is kept, and
// this package says plainly in its own header that it is "not a wall against a
// same-user attacker".
//
// On the install this was written for, the client keeps no config file at all
// (issue #25), so the token ended up in a user-level environment variable:
// readable by every process running as that user. Which is to say, readable by
// exactly the attacker in question. What still did real work was the parent
// binding — a secret copied from anywhere is useless if the program launching
// the server is not the recorded one.
//
// And the binding never needed the secret. Everything else — the config file,
// the environment variable, the confirmation trial, `--status`, `--enforce-now`
// — is machinery guarding a value that, in that setup, is public. It cost the
// user five steps and gave back nothing the binding did not already give.
//
// So: record who launches the server the first time, and refuse anyone else.
// The user runs nothing. There is no secret to leak, copy, rotate or lose.
//
// # What it does not do, stated where it can be read
//
//   - If the very first launch on a fresh install is the attacker, the attacker
//     is what gets recorded. Winning that race means beating the client to the
//     first start on a machine that has just been set up. The token did not
//     cover this either: whoever can read the environment reads it.
//   - A client that changes path when it updates falls outside until it is
//     trusted again. Same shape as ForeignParent today, except the fix is one
//     command and involves no secret.
//   - It does not tell two different clients apart the way labelled tokens do.
//     That granularity is traded for a mechanism people actually use.
//
// The token is not going anywhere: a record that holds tokens keeps behaving
// exactly as it did. This changes what happens when there is no record at all.

// learningWindow is how long a new record keeps adopting launchers instead of
// refusing them.
//
// WHY IT EXISTS, AND IT IS NOT COSMETIC
// The first version of this recorded the first launcher and refused everyone
// after. TestUnpairedServesAsBefore caught what that costs: someone whose
// server is started by two programs — a client and an editor, say — updates,
// the first one to start wins, and the other stops working. Its comment is
// right that bricking a working setup on upgrade is worse than the exposure
// being closed.
//
// So a fresh record spends its first day learning rather than judging. Every
// launcher seen in that window is adopted and announced; after it, the list is
// closed and anything new is refused. Someone with two clients uses both on the
// first day without noticing this exists, and an attacker who shows up later
// gets nothing.
//
// The window is a real gap and worth naming: something that launches the server
// within the first day of a fresh record is trusted, permanently. It is the same
// trade the token made and lost — there the secret sat in an environment
// variable any process could read, which is a door that never closes. This one
// shuts on its own.
const learningWindow = 24 * time.Hour

// Launcher is a program allowed to start this server without a token.
type Launcher struct {
	Exe string `json:"exe"`

	// Recorded is when this launcher was trusted, and FirstUse says whether it
	// got there by simply being first rather than by someone adding it. The
	// distinction is worth keeping: it is the difference between "the user
	// decided this" and "nobody decided anything and we took the default", and
	// it is what `trust --status` shows when someone asks why a program is on
	// the list.
	Recorded time.Time `json:"recorded"`
	FirstUse bool      `json:"first_use,omitempty"`
}

// TrustsLaunchers reports whether this record authorizes by launcher.
//
// A record with tokens keeps the token rules even if it also carries launchers:
// someone who went to the trouble of issuing a credential meant it, and quietly
// demoting them to the weaker check would be the kind of silent downgrade this
// package exists to avoid.
func (f *File) TrustsLaunchers() bool {
	return f != nil && len(f.Tokens) == 0 && len(f.TrustedLaunchers) > 0
}

// StillLearning reports whether this record is inside the window where new
// launchers are adopted rather than refused.
func (f *File) StillLearning(now time.Time) bool {
	if f == nil || len(f.TrustedLaunchers) == 0 {
		return false
	}
	// Measured from the oldest entry: the window belongs to the record, not to
	// each launcher. Otherwise adding one every 23 hours would keep it open for
	// ever, which is the same as never closing it.
	oldest := f.TrustedLaunchers[0].Recorded
	for _, l := range f.TrustedLaunchers[1:] {
		if l.Recorded.Before(oldest) {
			oldest = l.Recorded
		}
	}
	return now.Sub(oldest) < learningWindow
}

// Trusts reports whether exe is on the list.
func (f *File) Trusts(exe string) bool {
	if f == nil || exe == "" {
		return false
	}
	for _, l := range f.TrustedLaunchers {
		if sameExe(l.Exe, exe) {
			return true
		}
	}
	return false
}

// Trust adds exe to the list, and reports whether it was already there.
func (f *File) Trust(exe string, firstUse bool) (added bool) {
	if f == nil || exe == "" || f.Trusts(exe) {
		return false
	}
	f.TrustedLaunchers = append(f.TrustedLaunchers, Launcher{
		Exe:      exe,
		Recorded: time.Now().UTC(),
		FirstUse: firstUse,
	})
	return true
}

// Untrust removes exe from the list.
func (f *File) Untrust(exe string) (removed bool) {
	if f == nil {
		return false
	}
	keep := f.TrustedLaunchers[:0]
	for _, l := range f.TrustedLaunchers {
		if sameExe(l.Exe, exe) {
			removed = true
			continue
		}
		keep = append(keep, l)
	}
	f.TrustedLaunchers = keep
	return removed
}
