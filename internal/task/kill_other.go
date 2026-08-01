//go:build !windows

package task

import (
	"os/exec"
	"syscall"
	"time"
)

// procGuard owns the OS resources used to terminate a child and its descendants.
// On Unix a process group is enough: the child leads its own group and a signal
// to the negated pid reaches every member.
type procGuard struct{}

// newProcGuard prepares cancellation for cmd. It must be called before Start.
func newProcGuard(cmd *exec.Cmd) *procGuard {
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
	return &procGuard{}
}

// AfterStart has nothing to do once the group is set at spawn time.
func (g *procGuard) AfterStart(cmd *exec.Cmd) {}

// Close has no resources to release.
func (g *procGuard) Close() {}
