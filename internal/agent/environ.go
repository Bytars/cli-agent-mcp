// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// An MCP client launches this server as a child process, and some clients hand
// it a curated environment rather than the one a login shell would have.
//
// Claude Desktop on Windows is one of them. The environment it passes omits
// ProgramData, ComSpec, OS, COMPUTERNAME and SESSIONNAME, and reduces PATHEXT to
// ".CPL". That looks cosmetic. It is not.
//
// Microsoft's Win32 build of OpenSSH resolves its system configuration
// directory from %ProgramData% during platform initialisation — before it parses
// arguments, and before its logging subsystem exists. With the variable absent,
// ssh.exe exits 255 having written nothing to stdout or stderr. Not even
// `ssh -V` prints its banner. The identical binary works perfectly from an
// interactive shell, which makes the failure look like anything except what it
// is. Diagnosing it from the symptoms costs hours; every plausible culprit —
// packaging, missing console, code signing, the runtime doing the spawning — has
// to be eliminated one at a time.
//
// This server exists to spawn CLI agents, and those agents spawn tools of their
// own. A hole in the inherited environment therefore propagates all the way
// down and surfaces as an unexplainable silent failure several processes away
// from the process that actually has the hole.
//
// RepairedEnviron restores well-known system variables that are missing. Two
// rules keep it safe:
//
//   - It never overrides a variable the host did set. If the operator or the
//     client deliberately passed something, that value wins.
//   - It only injects a path after confirming that path exists on this machine,
//     so a wrong guess becomes a no-op rather than a new failure mode.

// defaultPathExt is the stock Windows value. It is only used when PATHEXT is
// absent entirely, never to "correct" one the host provided.
const defaultPathExt = ".COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC"

// RepairedEnviron returns this process's environment with missing system
// variables restored, plus the names of whatever it had to restore. The names
// are worth surfacing: they are a precise fingerprint of what the launching
// client failed to pass on.
func RepairedEnviron() (env []string, repaired []string) {
	return repairEnviron(os.Environ())
}

// repairEnviron is the testable core. base is in "NAME=VALUE" form.
func repairEnviron(base []string) (env []string, repaired []string) {
	env = append([]string(nil), base...)
	if runtime.GOOS != "windows" {
		return env, nil
	}

	present := make(map[string]string, len(base))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i > 0 {
			present[strings.ToUpper(kv[:i])] = kv[i+1:]
		}
	}
	lookup := func(name string) string { return present[strings.ToUpper(name)] }

	sysRoot := firstNonEmpty(lookup("SystemRoot"), lookup("windir"), `C:\Windows`)
	sysDrive := firstNonEmpty(lookup("SystemDrive"), filepath.VolumeName(sysRoot), "C:")
	programData := sysDrive + `\ProgramData`
	comSpec := filepath.Join(sysRoot, "system32", "cmd.exe")

	candidates := []struct{ name, value string }{
		// The one that started all this. ALLUSERSPROFILE is its historical
		// alias and costs nothing to restore alongside it.
		{"ProgramData", dirOrEmpty(programData)},
		{"ALLUSERSPROFILE", dirOrEmpty(programData)},

		{"ComSpec", fileOrEmpty(comSpec)},
		{"SystemRoot", dirOrEmpty(sysRoot)},
		{"windir", dirOrEmpty(sysRoot)},
		{"SystemDrive", sysDrive},

		// Without a usable PATHEXT, PATH look-ups silently stop finding .exe
		// files — `where ssh` reports nothing even though ssh.exe is on PATH.
		{"PATHEXT", defaultPathExt},
	}

	for _, c := range candidates {
		if c.value == "" {
			continue
		}
		if _, ok := present[strings.ToUpper(c.name)]; ok {
			continue // the host set it; leave it alone
		}
		env = append(env, c.name+"="+c.value)
		present[strings.ToUpper(c.name)] = c.value
		repaired = append(repaired, c.name)
	}

	sort.Strings(repaired)
	return env, repaired
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func dirOrEmpty(p string) string {
	if info, err := os.Stat(p); err == nil && info.IsDir() {
		return p
	}
	return ""
}

func fileOrEmpty(p string) string {
	if fileExists(p) {
		return p
	}
	return ""
}
