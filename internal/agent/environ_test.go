// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && strings.EqualFold(kv[:i], name) {
			return kv[i+1:], true
		}
	}
	return "", false
}

func TestRepairEnvironRestoresProgramData(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the repair set is Windows-specific")
	}
	// The exact shape observed from a client that curates the environment:
	// ProgramData and ComSpec gone, PATHEXT reduced to a single entry.
	base := []string{
		`SystemRoot=C:\Windows`,
		`windir=C:\Windows`,
		`SystemDrive=C:`,
		`PATHEXT=.CPL`,
		`USERPROFILE=C:\Users\someone`,
	}

	env, repaired := repairEnviron(base)

	if _, ok := envValue(env, "ProgramData"); !ok {
		t.Fatal("ProgramData must be restored: without it Windows OpenSSH exits 255 writing nothing")
	}
	if !containsFold(repaired, "ProgramData") {
		t.Errorf("ProgramData should be reported as repaired, got %v", repaired)
	}
}

func TestRepairEnvironNeverOverridesTheHost(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the repair set is Windows-specific")
	}
	// A value the host deliberately set must survive untouched, even if it is
	// unusual — second-guessing the operator is how a helper becomes a hazard.
	base := []string{
		`SystemRoot=C:\Windows`,
		`ProgramData=D:\CustomProgramData`,
		`PATHEXT=.CPL`,
	}

	env, repaired := repairEnviron(base)

	got, _ := envValue(env, "ProgramData")
	if got != `D:\CustomProgramData` {
		t.Errorf("host-provided ProgramData was overridden: got %q", got)
	}
	if containsFold(repaired, "ProgramData") {
		t.Error("a variable the host already set must not be reported as repaired")
	}
	if containsFold(repaired, "PATHEXT") {
		t.Error("a PATHEXT the host set must be left alone, however short it looks")
	}
}

func TestRepairEnvironIsANoOpWhenNothingIsMissing(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the repair set is Windows-specific")
	}
	base := []string{
		`SystemRoot=C:\Windows`,
		`windir=C:\Windows`,
		`SystemDrive=C:`,
		`ProgramData=C:\ProgramData`,
		`ALLUSERSPROFILE=C:\ProgramData`,
		`ComSpec=C:\Windows\system32\cmd.exe`,
		`PATHEXT=` + defaultPathExt,
	}

	env, repaired := repairEnviron(base)

	if len(repaired) != 0 {
		t.Errorf("a complete environment must need no repair, got %v", repaired)
	}
	if len(env) != len(base) {
		t.Errorf("a complete environment must not grow: %d entries became %d", len(base), len(env))
	}
}

func TestRepairEnvironLeavesNonWindowsAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this asserts the non-Windows path")
	}
	base := []string{"PATH=/usr/bin", "HOME=/home/someone"}
	env, repaired := repairEnviron(base)
	if len(repaired) != 0 {
		t.Errorf("no repairs apply off Windows, got %v", repaired)
	}
	if len(env) != len(base) {
		t.Error("the environment must be passed through unchanged")
	}
}

func TestRepairedEnvironPreservesTheRealEnvironment(t *testing.T) {
	// The whole point of inheriting the environment is that the worker reaches
	// what the host reaches. Repair must only ever add.
	env, _ := RepairedEnviron()
	if len(env) == 0 {
		t.Fatal("RepairedEnviron returned an empty environment")
	}
	for _, kv := range os.Environ() {
		if !contains(env, kv) {
			t.Errorf("repair dropped an inherited variable: %q", kv)
		}
	}
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
