// SPDX-License-Identifier: Apache-2.0

//go:build windows

package task

import (
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// Cancelling a task must stop everything the worker spawned, not just the
// launcher. A coding agent routinely starts a Node runtime, shells for tool
// calls, language servers — an orphan left behind keeps holding files, ports
// and credentials.
//
// `taskkill /F /T` walks the tree by parent PID, which means it only works if
// every intermediate process is still alive to be walked. Agents exit their
// launcher early all the time, and the grandchildren are then reparented and
// survive. A Job Object closes that particular hole: membership is inherited
// through normal process creation, so terminating the job also terminates
// descendants whose intermediate parent has already exited.
//
// What it does NOT cover, measured rather than assumed: a process launched via
// ShellExecuteEx — which is what PowerShell's `Start-Process` does by default —
// is created by the shell, not by us. It never joins our job, and it is not our
// descendant in any sense Windows tracks. A worker that deliberately detaches a
// process that way escapes this containment, as it would escape any
// parentage-based mechanism, taskkill included. Verified: a Start-Process
// grandchild was still running and writing after its turn ended.
//
// taskkill is kept as a fallback for the case where the job cannot be created
// or assigned, so behaviour never regresses below what it was.

const (
	jobObjectExtendedLimitInfoClass = 9
	jobObjectLimitKillOnJobClose    = 0x00002000

	processTerminate    = 0x0001
	processSetQuota     = 0x0100
	processQueryLimited = 0x1000
)

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW   = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJob  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJob = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject = kernel32.NewProc("TerminateJobObject")
	procOpenProcess        = kernel32.NewProc("OpenProcess")
)

// procGuard owns the OS resources used to terminate a child and its descendants.
type procGuard struct {
	job syscall.Handle
}

// newProcGuard prepares cancellation for cmd. It must be called before Start;
// AfterStart must be called once the process exists.
func newProcGuard(cmd *exec.Cmd) *procGuard {
	g := &procGuard{}

	// WaitDelay bounds how long Wait blocks on pipes after the process exits.
	cmd.WaitDelay = 5 * time.Second

	if h, err := createKillOnCloseJob(); err == nil {
		g.job = h
	}

	cmd.Cancel = func() error {
		if g.job != 0 {
			// Terminating the job takes every descendant with it, regardless of
			// whether the intermediate processes are still alive.
			r, _, err := procTerminateJobObject.Call(uintptr(g.job), 1)
			if r != 0 {
				return nil
			}
			_ = err
		}
		if cmd.Process == nil {
			return nil
		}
		return exec.Command(taskkillPath(), "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
	return g
}

// AfterStart puts the freshly started process into the job so that every
// descendant it creates from now on is a member too.
//
// There is an unavoidable sliver of time between Start and this call in which a
// grandchild could be created outside the job. It is microseconds wide, and the
// taskkill fallback still covers that process if it is a direct descendant.
// Closing it entirely would require starting the process suspended, which Go's
// os/exec does not expose.
func (g *procGuard) AfterStart(cmd *exec.Cmd) {
	if g.job == 0 || cmd.Process == nil {
		return
	}
	h, _, _ := procOpenProcess.Call(
		uintptr(processTerminate|processSetQuota|processQueryLimited),
		0,
		uintptr(cmd.Process.Pid),
	)
	if h == 0 {
		return
	}
	defer syscall.CloseHandle(syscall.Handle(h))
	procAssignProcessToJob.Call(uintptr(g.job), h)
}

// Close releases the job handle. Because the job was created with
// KILL_ON_JOB_CLOSE, this must only happen once the turn is over.
func (g *procGuard) Close() {
	if g.job != 0 {
		syscall.CloseHandle(g.job)
		g.job = 0
	}
}

func createKillOnCloseJob() (syscall.Handle, error) {
	r, _, err := procCreateJobObjectW.Call(0, 0)
	if r == 0 {
		return 0, err
	}
	h := syscall.Handle(r)

	var info jobObjectExtendedLimitInformation
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	ok, _, err := procSetInformationJob.Call(
		uintptr(h),
		uintptr(jobObjectExtendedLimitInfoClass),
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if ok == 0 {
		syscall.CloseHandle(h)
		return 0, err
	}
	return h, nil
}

// taskkillPath anchors taskkill to System32 rather than trusting PATH.
func taskkillPath() string {
	if root := syscallGetenv("SystemRoot"); root != "" {
		return root + `\System32\taskkill.exe`
	}
	return "taskkill"
}
