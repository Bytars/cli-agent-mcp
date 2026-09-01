// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"regexp"
	"strings"
)

// Identity is what a launcher is compared by — and it is deliberately NOT its
// path.
//
// # The path is not stable, and that cost a working day
//
// Claude Desktop ships as an MSIX package, so its executable lives at
//
//	...\WindowsApps\Claude_1.40609.0.0_x64__pzs8sxrjxfjjc\app\claude.exe
//
// with the version number inside the path. The app updated itself in the
// background, the path changed to `1.40609.1.0`, and the binding broke:
//
//	token "cowork" is bound to   ...Claude_1.40609.0.0...\app\claude.exe
//	but this server was launched by ...Claude_1.40609.1.0...\app\claude.exe
//
// The user did nothing. An application updated in the background and the MCP
// server stopped working — and every rescue command made it worse, because
// they had their own problems (see issue #29).
//
// # What is stable
//
// For an MSIX package, the identity is the *package family*: `Claude` plus the
// publisher hash `pzs8sxrjxfjjc`. That pair survives every version bump, which
// is exactly the property a binding needs. Outside MSIX there is no such
// notion, so this falls back to the executable name — coarser, but it also
// survives an in-place update, which the full path does not.
//
// # Coarser on purpose
//
// Comparing `claude.exe` rather than a full path means a program of the same
// name somewhere else also matches. That is a real loss, and it is the right
// trade: the alternative is what happened here, where the legitimate client
// locked itself out by updating. The attacker this defends against — code that
// can execute but cannot rummage through the profile — is not helped much by
// the extra precision, and the user is hurt a lot by it.
type Identity struct {
	// Kind is "msix" or "exe", and says how Value should be read.
	Kind string `json:"kind"`

	// Value is the package family for msix, the executable name for exe.
	Value string `json:"value"`

	// Path is the full path this identity was derived from, kept only so a
	// person reading the record or a rejection can recognise the program. It is
	// never compared.
	Path string `json:"path,omitempty"`
}

// msixPackageDir matches a WindowsApps package folder and captures the two
// parts that do not change between versions: the package name and the
// publisher hash.
//
//	Claude_1.40609.1.0_x64__pzs8sxrjxfjjc
//	^^^^^^                   ^^^^^^^^^^^^
var msixPackageDir = regexp.MustCompile(`^([^_]+)_[0-9][^_]*_[^_]+__(.+)$`)

// IdentityOf derives the comparable identity of an executable path.
//
// An empty path yields a zero Identity, which never matches anything — callers
// treat "cannot identify the launcher" as a reason to serve, not to refuse
// (issue #29: the mechanism must not be able to lock the user out).
func IdentityOf(exe string) Identity {
	if strings.TrimSpace(exe) == "" {
		return Identity{}
	}
	segs := segments(exe)
	if len(segs) == 0 {
		return Identity{}
	}
	// Walk the package folders rather than assuming a depth: the executable can
	// sit at the package root or under app\, and both occur.
	for _, s := range segs {
		if m := msixPackageDir.FindStringSubmatch(s); m != nil {
			return Identity{Kind: "msix", Value: m[1] + "_" + m[2], Path: exe}
		}
	}
	return Identity{Kind: "exe", Value: strings.ToLower(segs[len(segs)-1]), Path: exe}
}

// segments splits a path on BOTH separators, independently of the one this
// build happens to use.
//
// Not filepath: this parses Windows paths, and it has to give the same answer
// wherever it runs. The tests for this package are cross-compiled to Linux —
// WDAC on the machine it was written for refuses to execute a freshly built
// test binary — and filepath.Base on Linux treats `C:\a\b.exe` as one segment,
// so every identity came out as the whole path and every comparison failed.
//
// That is worth more than a portability note: a stored record is JSON, and
// nothing stops a Windows path from being read back by a build for another
// platform. Parsing that depends on where the code runs would quietly disagree
// with itself.
func segments(p string) []string {
	out := make([]string, 0, 8)
	for _, s := range strings.FieldsFunc(p, func(r rune) bool { return r == '\\' || r == '/' }) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Matches reports whether two identities refer to the same program.
//
// A zero identity matches nothing, on either side. That is what makes "the
// platform could not name the launcher" fall through to serving rather than
// silently matching everything or refusing everyone.
func (i Identity) Matches(other Identity) bool {
	if i.Kind == "" || other.Kind == "" || i.Value == "" || other.Value == "" {
		return false
	}
	if i.Kind != other.Kind {
		return false
	}
	if i.Kind == "msix" {
		// Package families are case-preserving but compared case-insensitively,
		// like everything else Windows names.
		return strings.EqualFold(i.Value, other.Value)
	}
	return strings.EqualFold(i.Value, other.Value)
}

// String renders an identity for a message a person has to act on.
func (i Identity) String() string {
	switch {
	case i.Kind == "":
		return "an unidentified program"
	case i.Kind == "msix":
		return "the app package " + i.Value
	default:
		return i.Value
	}
}
