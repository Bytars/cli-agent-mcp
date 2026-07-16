//go:build windows

package task

import (
	"os/exec"
	"strconv"
	"time"
)

// configureCancel makes cancelling a task kill the agent's whole process tree.
//
// The default exec.CommandContext behaviour only terminates the direct child,
// which on Windows can orphan grandchildren (an agent often spawns Node, shells
// for tool calls, etc.). For an interrupt to actually stop everything we kill
// the tree with `taskkill /T`.
func configureCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
}
