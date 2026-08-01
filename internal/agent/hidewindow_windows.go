//go:build windows

package agent

import (
	"os/exec"
	"syscall"
)

// systemRoot returns the Windows directory, defaulting to the conventional path
// when the environment variable is missing.
func systemRoot() string {
	if r := getenv("SystemRoot"); r != "" {
		return r
	}
	return `C:\Windows`
}

// hardenSpawn applies the Windows-specific spawn settings this server always
// wants: no console window, and a new process group so the child can be
// signalled as a unit.
//
// CREATE_NO_WINDOW matters more than it looks. This server is normally launched
// by a GUI application that has no console of its own; without it, every child
// either flashes a console window at the user or inherits a console handle that
// may not exist.
func hardenSpawn(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow | createNewProcessGroup
	return cmd
}

const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)
