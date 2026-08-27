// SPDX-License-Identifier: Apache-2.0

//go:build windows

package pairing

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Windows has no getppid: a process does not carry a live parent pointer, so
// the parent PID has to be read out of a process snapshot. The snapshot is also
// why this is best-effort — by the time it is taken, the recorded parent PID
// may already belong to a process that exited, and PIDs are reused. Neither
// matters for the check performed here: a wrong answer can only fail to match a
// recorded binding, which is reported to the user with a way out, never
// silently accepted as a match.
const (
	th32csSnapProcess              = 0x00000002
	processQueryLimitedInformation = 0x1000
	errNoMoreFiles                 = 8
	maxPath                        = 260
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procCreateToolhelp32Snapshot   = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW            = kernel32.NewProc("Process32FirstW")
	procProcess32NextW             = kernel32.NewProc("Process32NextW")
	procOpenProcess                = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
)

// processEntry32 mirrors PROCESSENTRY32W. The layout is fixed by the API; the
// leading Size field must be set before the first call or the enumeration fails.
type processEntry32 struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [maxPath]uint16
}

func parentExe() (string, error) {
	ppid, err := parentPID(uint32(os.Getpid()))
	if err != nil {
		return "", err
	}
	return imageName(ppid)
}

// parentPID walks a process snapshot looking for pid's entry.
func parentPID(pid uint32) (uint32, error) {
	snap, _, err := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == uintptr(syscall.InvalidHandle) {
		return 0, fmt.Errorf("process snapshot: %w", err)
	}
	defer syscall.CloseHandle(syscall.Handle(snap))

	var e processEntry32
	e.Size = uint32(unsafe.Sizeof(e))

	r, _, err := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&e)))
	for r != 0 {
		if e.ProcessID == pid {
			return e.ParentProcessID, nil
		}
		r, _, err = procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&e)))
	}
	if errno, ok := err.(syscall.Errno); ok && errno == errNoMoreFiles {
		return 0, errors.New("this process is not in the process snapshot")
	}
	return 0, fmt.Errorf("walk process snapshot: %w", err)
}

// imageName asks the kernel for a process's executable path.
//
// PROCESS_QUERY_LIMITED_INFORMATION is the least privilege that answers this,
// and unlike PROCESS_QUERY_INFORMATION it is granted across integrity levels —
// which matters because the client that launched us need not run at the same
// level we do.
func imageName(pid uint32) (string, error) {
	if pid == 0 {
		return "", errors.New("no parent process")
	}
	h, _, err := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return "", fmt.Errorf("open parent process %d: %w", pid, err)
	}
	defer syscall.CloseHandle(syscall.Handle(h))

	// Long paths exist; ask for room well past MAX_PATH rather than truncating
	// a launcher installed deep in a user profile.
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	r, _, err := procQueryFullProcessImageNameW.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return "", fmt.Errorf("query parent image name: %w", err)
	}
	return syscall.UTF16ToString(buf[:size]), nil
}
