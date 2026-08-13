// SPDX-License-Identifier: Apache-2.0

//go:build windows

package state

import (
	"syscall"
	"unsafe"
)

// Windows has no signal-0 liveness probe, so ask the kernel directly whether
// the process still has an exit code pending.
//
// Two known imprecisions, both acceptable for deciding whether to print a
// warning: a PID can be reused by an unrelated process, and a process that
// genuinely exited with code 259 is indistinguishable from a running one.
// Neither can turn a real conflict into silence, which is the failure that
// would actually matter here.
const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess        = kernel32.NewProc("OpenProcess")
	procGetExitCodeProcess = kernel32.NewProc("GetExitCodeProcess")
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	defer syscall.CloseHandle(syscall.Handle(h))

	var code uint32
	r, _, _ := procGetExitCodeProcess.Call(h, uintptr(unsafe.Pointer(&code)))
	return r != 0 && code == stillActive
}
