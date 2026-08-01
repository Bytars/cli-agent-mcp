//go:build !windows

package state

import "syscall"

// Signal 0 performs the permission and existence checks without delivering
// anything. EPERM means the process exists but belongs to another user, which
// still counts as alive for our purposes.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
