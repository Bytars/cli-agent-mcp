// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Telling the user, from the terminal, that the server reads another file.
//
// # The mirror
//
// Claude Desktop ships as an MSIX package, and MSIX virtualizes %APPDATA%: a
// process inside the package writing to Roaming lands in its own private copy,
//
//	%LOCALAPPDATA%\Packages\Claude_pzs8sxrjxfjjc\LocalCache\Roaming\cli-agent-mcp\
//
// while the same binary run from a terminal writes to
//
//	%APPDATA%\cli-agent-mcp\
//
// **They are never the same file.** On 1-sep that cost a working day: the
// server was enforcing a token while `pair --status` printed NOT PAIRED, and
// four rescue attempts in a row changed nothing. Both sides reported success
// about different files, and nothing on either side named the file it meant.
//
// # What this does, and what it deliberately does not
//
// It does not fix the divergence — the two processes genuinely see different
// values for the same variable, and no amount of agreeing on a name closes
// that. It makes the divergence impossible to be fooled by: before `--status`
// answers, it looks for a record on the other side of the mirror and, if one is
// there, names both paths and says which of the two this command is about to
// act on.
//
// It is a warning and never a refusal. Acting on the terminal's own record is a
// perfectly ordinary thing to do — this only stops it from being mistaken for
// acting on the server's.
//
// # It never reads the other record, only notices it
//
// The file on the other side of the mirror is written by whatever runs as this
// user, and a record with a format version this build does not understand
// already makes the SERVER refuse everyone. Parsing it here to say something
// nicer about it would hand that same lever to `--status`: a file somebody else
// wrote could turn a read-only question into an error, or into advice about a
// pairing that is not there. Existence is the whole claim being made — there is
// a record over there, and it is not this one — and existence is all this
// looks at.

// stateDirName is the per-user directory the state package builds under the
// user's config root (see state.DefaultDir). It is repeated rather than
// imported because this file has to spell out the virtualized layout by hand
// anyway: %APPDATA% is exactly what MSIX rewrites, so the only way to reach the
// package's copy is to construct the path from %LOCALAPPDATA% instead.
const stateDirName = "cli-agent-mcp"

// packagedRecords lists every pairing record sitting where an MSIX-packaged
// app's server would read it.
//
// # Why the environment variable and not runtime.GOOS
//
// The mechanism here is %LOCALAPPDATA%\Packages, not the string "windows". A
// GOOS guard would compile the search out of the Linux build, which is half of
// CI — the twin tests below (it warns / it stays quiet) would then assert
// nothing there, and a regression could land green. Keying on the variable
// keeps one code path, exercised on both legs, and it is inert wherever the
// variable is unset, which is every ordinary Unix machine.
func packagedRecords() []string {
	root := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if root == "" {
		return nil
	}
	// One glob rather than a walk: the shape is fixed by MSIX, and walking
	// %LOCALAPPDATA%\Packages means descending into hundreds of app trees to
	// answer a question about one directory each.
	found, err := filepath.Glob(filepath.Join(root, "Packages", "*", "LocalCache", "Roaming", stateDirName, FileName))
	if err != nil {
		// The only error Glob reports is a malformed pattern, and this pattern
		// is a constant. Nothing to tell the user, and a warning that cannot be
		// produced must not become an error that stops --status from answering.
		return nil
	}
	sort.Strings(found)
	return found
}

// virtualizedRecordWarning reports that the record this command resolved is not
// the one a packaged app's server reads. It returns "" when there is nothing to
// warn about — including, and this is the case that matters, when the resolved
// directory IS the virtualized one, which is what happens after someone follows
// the --state-dir instruction the rejection now prints.
func virtualizedRecordWarning(stateDir string) string {
	mine := Path(stateDir)
	var others []string
	for _, p := range packagedRecords() {
		if !sameRecordFile(p, mine) {
			others = append(others, p)
		}
	}
	if len(others) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("WARNING: this is not the record a packaged app's server reads.\n\n")
	fmt.Fprintf(&b, "  this command acts on:  %s\n", mine)
	for _, p := range others {
		fmt.Fprintf(&b, "  a packaged app reads:  %s\n", p)
	}
	b.WriteString(`
Claude Desktop ships as an MSIX package, and MSIX virtualizes %APPDATA%: a
server it launches sees its own private copy of Roaming, which is the second
path above. This terminal sees the first. They are never the same file, so
whatever this command reports or changes says NOTHING about that server — it
will keep enforcing, or keep serving, exactly as before.

To act on the record that server reads, name it:

`)
	for _, p := range others {
		// Quoted by hand, not with %q: %q escapes every backslash, and a
		// Windows path came out as C:\\Users\\… — which is what the reader
		// pastes, and it is not the same directory. The one instruction this
		// warning exists to give would have been wrong in the only place it
		// gets used. Caught by running the binary, then pinned by a test.
		fmt.Fprintf(&b, "    --state-dir \"%s\"\n", filepath.Dir(p))
	}
	b.WriteString("\n")
	return b.String()
}

// sameRecordFile compares two record paths the way the filesystem holding them
// would.
//
// Not sameExe: that one compares program identity — a package family, or a bare
// executable name — which would call two different files with the same name the
// same thing. Here the whole question is whether these are literally the same
// file, and answering it loosely would suppress precisely the warning this
// exists to print.
//
// Separators are normalised rather than left to filepath, and the comparison is
// case-insensitive, because both sides of it are Windows paths by construction
// while the tests for this package also run on Linux.
func sameRecordFile(a, b string) bool {
	return strings.EqualFold(normalizeRecordPath(a), normalizeRecordPath(b))
}

func normalizeRecordPath(p string) string {
	return strings.Join(segments(p), `\`)
}
