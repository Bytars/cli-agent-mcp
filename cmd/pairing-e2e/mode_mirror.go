// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	register("mirror", mode{
		Point: 2,
		What: "MSIX virtualizes %APPDATA%, so the server inside the package and the same command typed at a\n" +
			"terminal read different files and both report success about them. --status has to name the\n" +
			"other side of the mirror instead of answering confidently about the wrong record.",
		Run: runMirror,
	})
}

// runMirror covers the divergence that made a whole day of rescue attempts
// unfalsifiable: the server was enforcing a token while `pair --status` printed
// NOT PAIRED, four rescues in a row changed nothing, and neither side ever
// named the file it meant.
//
// It is the one mode that needs no launcher. The failure is entirely about
// which path a command resolves, so it is driven by running `pair` the way a
// person at a terminal does — which is where the wrong answer was given.
func runMirror(r *rig) {
	// The mirror, built by hand. LOCALAPPDATA is pointed at a root that has the
	// packaged layout under it; packagedRecords globs for exactly this shape.
	local := filepath.Join(r.dir, "mirror", "Local")
	virtual := filepath.Join(local, "Packages", "Claude_pzs8sxrjxfjjc", "LocalCache", "Roaming", "cli-agent-mcp")
	roaming := filepath.Join(r.dir, "mirror", "Roaming", "cli-agent-mcp")
	for _, d := range []string{virtual, roaming} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			r.check("mirror directories could be created", "^$", err.Error())
			return
		}
	}
	// LOCALAPPDATA has to be set for the CHILD, not for this process: it is what
	// the server's own lookup reads, and pointing it here would be pointing it
	// at the machine's real Packages tree.
	inPackage := "LOCALAPPDATA=" + local

	// A record on the packaged side — the one the server would actually read.
	// CLI_AGENT_MCP_STATE_DIR is left empty so only the explicit --state-dir
	// decides, which is the situation the warning is about.
	r.run(r.env("", inPackage), r.server, "pair", "--label", "x", "--state-dir", virtual)

	// Asked about the terminal's own record, with the packaged one sitting there.
	out := r.run(r.env("", inPackage), r.server, "pair", "--status", "--state-dir", roaming)
	r.check("says the server reads another file",
		"WARNING: this is not the record", out)
	// Warning about it and not saying WHERE is what the original messages did.
	r.check("and names the path that matters",
		"LocalCache", out)

	// THE CONTROL. Same command, same environment, aimed at the packaged record
	// itself — there is no divergence to report, so there must be no warning. A
	// warning that fires either way is noise, and noise is what teaches people
	// to ignore the one that is true.
	quiet := r.run(r.env("", inPackage), r.server, "pair", "--status", "--state-dir", virtual)
	r.check("CONTROL: matching paths stay quiet",
		"^0$", fmt.Sprint(strings.Count(quiet, "WARNING")))
}
