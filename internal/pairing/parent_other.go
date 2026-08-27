// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package pairing

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// parentExe resolves the launcher's executable.
//
// Linux and the other procfs systems answer straight from /proc. macOS has no
// procfs and the C call that would answer directly (proc_pidpath) needs cgo,
// which this build deliberately does not use, so it falls back to ps — whose
// `comm` column on Darwin is the full executable path. The fallback runs once
// at startup and is bounded, and returning nothing simply means the binding
// layer stays off for that token.
func parentExe() (string, error) {
	ppid := os.Getppid()
	if ppid <= 1 {
		return "", errors.New("no parent process")
	}

	if exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(ppid), "exe")); err == nil && exe != "" {
		// A deleted binary comes back as "/path/to/thing (deleted)".
		return strings.TrimSuffix(exe, " (deleted)"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "comm=", "-p", strconv.Itoa(ppid)).Output()
	if err != nil {
		return "", err
	}
	exe := strings.TrimSpace(string(out))
	if exe == "" {
		return "", errors.New("ps reported no command for the parent process")
	}
	return exe, nil
}
