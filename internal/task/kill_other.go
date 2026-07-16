//go:build !windows

package task

import (
	"os/exec"
	"syscall"
	"time"
)

// configureCancel makes cancelling a task kill the agent's whole process group,
// not just the direct child, so an interrupt actually stops everything the
// worker spawned (shells for tool calls, subprocesses, etc.).
func configureCancel(cmd *exec.Cmd) {
	// Put the child in its own process group so we can signal the whole group.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true

	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid signals the whole process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
