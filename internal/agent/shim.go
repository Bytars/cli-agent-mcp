package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Windows script shims are the default way npm installs a CLI: `npm i -g
// @anthropic-ai/claude-code` puts a `claude.cmd` (and `claude.ps1`) on PATH that
// does nothing but locate a Node runtime and hand it a .js entry point.
//
// Running those shims through `cmd /c` or `powershell -File` is the single worst
// thing this process can do on Windows, for two independent reasons:
//
//  1. Quoting. Go escapes arguments for CommandLineToArgvW, but cmd.exe uses a
//     different parser that does not understand `\"`. A prompt containing a
//     double quote can therefore terminate the quoted region early and let the
//     rest be reinterpreted as shell syntax. Prompts here are model-generated,
//     so this is reachable input. Go's CVE-2024-24576 mitigation does not cover
//     it: that fix triggers when the *target* is a .bat/.cmd, and here the
//     target is cmd.exe itself.
//
//  2. Fragility under packaged hosts. When the MCP server is launched by an
//     MSIX-packaged application (Claude Desktop on Windows is one), child
//     processes inherit package identity and a console-less environment. Extra
//     interpreter hops are extra chances to die without producing any output.
//
// resolveScriptShim reads the shim and extracts what it would have run, so we
// can invoke `node.exe entry.js ...` directly — one process, argv passed
// verbatim, no shell parser anywhere in the path. This mirrors what the Cursor
// adapter already did by hand for its bundled runtime.

// shimJSRefRE matches a quoted path ending in .js inside a shim script.
var shimJSRefRE = regexp.MustCompile(`["']([^"']*\.js)["']`)

// shimBaseVars are the variables npm shims use for "the directory I live in".
var shimBaseVars = []string{"%~dp0", "%dp0%", "$basedir", "${basedir}", "%~dp0%"}

const maxShimBytes = 128 * 1024

// resolveScriptShim inspects a .cmd/.bat/.ps1 launcher and, if it is a thin
// wrapper around a Node entry point, returns the node binary and that entry.
// ok is false for anything it cannot resolve with confidence — callers must
// keep their existing behaviour in that case rather than guessing.
func resolveScriptShim(launcher string) (node, entry string, ok bool) {
	switch strings.ToLower(filepath.Ext(launcher)) {
	case ".cmd", ".bat", ".ps1":
	default:
		return "", "", false
	}

	info, err := os.Stat(launcher)
	if err != nil || info.IsDir() || info.Size() > maxShimBytes {
		return "", "", false
	}
	data, err := os.ReadFile(launcher)
	if err != nil {
		return "", "", false
	}

	dir := filepath.Dir(launcher)
	for _, m := range shimJSRefRE.FindAllStringSubmatch(string(data), -1) {
		cand := expandShimBase(m[1], dir)
		if cand == "" {
			continue
		}
		if !filepath.IsAbs(cand) {
			cand = filepath.Join(dir, cand)
		}
		cand = filepath.Clean(cand)
		if !fileExists(cand) {
			continue
		}
		if n := findNodeFor(dir); n != "" {
			return n, cand, true
		}
		return "", "", false
	}
	return "", "", false
}

// expandShimBase substitutes the shim's "my directory" variable. A reference we
// do not recognise but that still contains a variable marker is rejected: a
// half-expanded path is worse than no path.
func expandShimBase(ref, dir string) string {
	out := ref
	for _, v := range shimBaseVars {
		out = strings.ReplaceAll(out, v, dir+string(filepath.Separator))
	}
	out = strings.ReplaceAll(out, "/", string(filepath.Separator))
	out = strings.ReplaceAll(out, string(filepath.Separator)+string(filepath.Separator), string(filepath.Separator))
	if strings.ContainsAny(out, "%$") {
		return ""
	}
	return out
}

// findNodeFor locates the Node runtime a shim would use: npm prefers one sitting
// next to the shim, then falls back to PATH.
func findNodeFor(dir string) string {
	local := filepath.Join(dir, nodeBinaryName())
	if fileExists(local) {
		return local
	}
	if p, err := exec.LookPath(nodeBinaryName()); err == nil {
		return p
	}
	if p, err := exec.LookPath("node"); err == nil {
		return p
	}
	return ""
}
