// SPDX-License-Identifier: Apache-2.0

package pairing

import "time"

// Announcing a refusal before making it.
//
// # The failure this closes
//
// Point 4 of issue #29: a launcher could go from "served yesterday" to
// "refused today" with nothing in between. That is what a background client
// update did — the app rewrote its own path, the binding stopped matching, and
// the first the user heard of it was every tool in the server being gone.
//
// From inside a client, a refusal with no warning before it is
// indistinguishable from the software being broken: no dialog, no reason, a
// server that used to work and now does not. Six lockouts over two days were
// diagnosed as crashes, bad paths and a missing binary before anyone suspected
// pairing at all, and each wrong guess cost another rescue attempt.
//
// So a launcher that is about to be locked out gets one start that says so —
// served, loudly, with the command that makes it permanent — and only the start
// after that is refused. The user hears "this will stop working, here is how to
// keep it" while they still have a working client to hear it in.
//
// # What it costs, stated rather than hidden
//
// A launcher that should be refused gets one free start. That is a real
// concession and it is the right one: the attacker this defends against — code
// that can execute but not rummage through the profile — gains a single session
// it has to use before anyone reads the warning, while the legitimate user
// gains the one thing every previous lockout lacked, which is notice. Issue #29
// settled the tie: if in doubt, serve and shout.
//
// # Why the mark does not expire
//
// Nothing ages an announcement out. It could — a mark that lapsed after a week
// would forgive a program that stayed away — and that is precisely the reason
// not to: it would hand a patient caller one free start per week for ever,
// which is a door that never closes. A legitimate launcher clears its own mark
// by being authorized, which is the only event that actually answers the
// question the mark asks.

// AnnouncedLauncher is a launcher that was served with a warning instead of
// being refused, and that the next start will refuse.
type AnnouncedLauncher struct {
	// ID is what the next start compares against, and it is an identity rather
	// than a path for the reason identity.go spells out: an MSIX path carries
	// the client's version number, so a mark keyed by path would warn about one
	// string and refuse a different one after the next background update —
	// re-creating, inside the very mechanism meant to prevent it, a refusal
	// nobody was warned about.
	ID Identity `json:"id"`

	// Announced is when the warning went out, and Reason names the refusal that
	// was held back ("foreign_launcher" or "foreign_parent"). The check reads
	// neither. They are here because this record is the only evidence that the
	// user was warned at all, and "you were told" is not a claim anyone can
	// stand behind if the file cannot say when, or about what.
	Announced time.Time `json:"announced"`
	Reason    string    `json:"reason,omitempty"`
}

// WasAnnounced reports whether this identity has already had its one warning,
// and may therefore be refused.
//
// A zero identity is never announced, because Matches refuses to match one on
// either side. That is deliberate and it points the same way as everything else
// here: a launcher the platform could not name must not be refused on the
// strength of a mark nobody can prove belongs to it.
func (f *File) WasAnnounced(id Identity) bool {
	if f == nil {
		return false
	}
	for _, a := range f.AnnouncedLaunchers {
		if a.ID.Matches(id) {
			return true
		}
	}
	return false
}

// Announce records that id was served with a warning, and reports whether that
// was new.
//
// A repeat call leaves the original timestamp alone. Refreshing it would be
// worse than useless: the mark is what makes the NEXT start a refusal, so the
// only thing a newer date could do is misreport when the user was actually told.
func (f *File) Announce(id Identity, reason string) (added bool) {
	if f == nil || id.Kind == "" || id.Value == "" || f.WasAnnounced(id) {
		return false
	}
	f.AnnouncedLaunchers = append(f.AnnouncedLaunchers, AnnouncedLauncher{
		ID:        id,
		Announced: time.Now().UTC(),
		Reason:    reason,
	})
	return true
}

// ForgetAnnouncement drops the mark for id, and reports whether one was there.
//
// Called wherever an identity becomes legitimately authorized — added to the
// trusted list, or bound to a token on first use. Without it the mark outlives
// the question it asked: the user runs the rescue command, the program is
// authorized, and the record still carries a note saying the next start should
// be refused. That note is one `trust --remove` or one `--unbind` away from
// being acted on, and it would then refuse a program the user has explicitly
// approved since — with no warning, which is the entire failure this file
// exists to prevent.
func (f *File) ForgetAnnouncement(id Identity) (removed bool) {
	if f == nil {
		return false
	}
	keep := f.AnnouncedLaunchers[:0]
	for _, a := range f.AnnouncedLaunchers {
		if a.ID.Matches(id) {
			removed = true
			continue
		}
		keep = append(keep, a)
	}
	f.AnnouncedLaunchers = keep
	return removed
}
