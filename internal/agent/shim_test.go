// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// npmCmdShim is the shape npm generates on Windows for a bin entry. The exact
// text matters: the resolver has to survive this without a shell.
const npmCmdShim = `@ECHO off
GOTO start
:find_dp0
SET dp0=%~dp0
EXIT /b
:start
SETLOCAL
CALL :find_dp0

IF EXIST "%dp0%\node.exe" (
  SET "_prog=%dp0%\node.exe"
) ELSE (
  SET "_prog=node"
  SET PATHEXT=%PATHEXT:;.JS;=;%
)

endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & "%_prog%"  "%dp0%\node_modules\@anthropic-ai\claude-code\cli.js" %*
`

// npmExeShim is the shape npm generates when the package ships a compiled
// launcher instead of a .js entry point — which is what @anthropic-ai/claude-code
// installs today. Reproduced byte for byte from a real claude.cmd, because
// failing to resolve this exact file is what made every prompt containing a
// quote fail outright.
const npmExeShim = `@ECHO off
GOTO start
:find_dp0
SET dp0=%~dp0
EXIT /b
:start
SETLOCAL
CALL :find_dp0
"%dp0%\node_modules\@anthropic-ai\claude-code\bin\claude.exe"   %*
`

func writeShimTree(t *testing.T) (dir, shim, entry, node string) {
	t.Helper()
	dir = t.TempDir()

	node = filepath.Join(dir, nodeBinaryName())
	if err := os.WriteFile(node, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry = filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "cli.js")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("// entry"), 0o644); err != nil {
		t.Fatal(err)
	}
	shim = filepath.Join(dir, "claude.cmd")
	if err := os.WriteFile(shim, []byte(npmCmdShim), 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

func TestResolveScriptShim_npmCmd(t *testing.T) {
	if runtime.GOOS != "windows" {
		// The path separators baked into an npm .cmd shim are Windows-only, so
		// the substitution can only be exercised faithfully there.
		t.Skip("windows-only shim format")
	}
	_, shim, entry, node := writeShimTree(t)

	gotNode, prefix, ok := resolveScriptShim(shim)
	if !ok {
		t.Fatalf("expected the shim to resolve")
	}
	if gotNode != node {
		t.Errorf("exe = %q, want %q", gotNode, node)
	}
	if len(prefix) != 1 || prefix[0] != entry {
		t.Errorf("prefix = %q, want [%q]", prefix, entry)
	}
}

// A shim that execs a bundled binary must resolve to that binary, with no
// arguments of its own inserted ahead of the caller's.
func TestResolveScriptShim_npmExeLauncher(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only shim format")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "claude.cmd")
	if err := os.WriteFile(shim, []byte(npmExeShim), 0o644); err != nil {
		t.Fatal(err)
	}

	exe, prefix, ok := resolveScriptShim(shim)
	if !ok {
		t.Fatalf("expected the exe shim to resolve")
	}
	if exe != target {
		t.Errorf("exe = %q, want %q", exe, target)
	}
	if len(prefix) != 0 {
		t.Errorf("prefix = %q, want none", prefix)
	}
}

// The Node runtime a shim uses to launch its entry point is not the program the
// shim wraps: resolving to it would run node with the caller's arguments and no
// script at all.
func TestResolveScriptShim_neverResolvesToNode(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only shim format")
	}
	dir, shim, _, node := writeShimTree(t)
	_ = dir
	exe, prefix, ok := resolveScriptShim(shim)
	if !ok {
		t.Fatal("expected the shim to resolve")
	}
	if exe == node && len(prefix) == 0 {
		t.Error("resolved to the node runtime with no entry point")
	}
}

func TestResolveScriptShim_rejectsNonShims(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := resolveScriptShim(exe); ok {
		t.Error("a real executable must not be treated as a shim")
	}
	if _, _, ok := resolveScriptShim(filepath.Join(dir, "missing.cmd")); ok {
		t.Error("a missing file must not resolve")
	}
}

func TestResolveScriptShim_rejectsUnresolvableEntry(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "thing.cmd")
	// References a .js that does not exist: resolving to a bogus path would be
	// worse than falling back.
	if err := os.WriteFile(shim, []byte(`"%dp0%\nope.js" %*`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := resolveScriptShim(shim); ok {
		t.Error("must not resolve to a non-existent entry point")
	}
}

func TestExpandShimBase_rejectsHalfExpanded(t *testing.T) {
	if got := expandShimBase(`%UNKNOWN%\cli.js`, `C:\x`); got != "" {
		t.Errorf("a reference with an unresolved variable must be rejected, got %q", got)
	}
}

func TestUnsafeForShell(t *testing.T) {
	safe := []string{"-p", "arregla el bug del login", "--model", "sonnet"}
	if bad := unsafeForShell(safe); bad != "" {
		t.Errorf("plain arguments must be accepted, rejected %q", bad)
	}
	for _, arg := range []string{
		`say "hola"`,  // quote: terminates cmd.exe quoting early
		"echo %PATH%", // expanded even inside quotes
		"bang!var!",   // delayed expansion
	} {
		if unsafeForShell([]string{"-p", arg}) == "" {
			t.Errorf("argument %q must be rejected for interpreter dispatch", arg)
		}
	}
}
