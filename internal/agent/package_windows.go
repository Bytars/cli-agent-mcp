// SPDX-License-Identifier: Apache-2.0

//go:build windows

package agent

import (
	"strings"
	"syscall"
	"unsafe"
)

// appmodelErrorNoPackage is what GetCurrentPackageFamilyName returns when the
// process has no package identity.
const appmodelErrorNoPackage = 15700

// packageIdentity reports whether this process runs with MSIX package identity,
// and the package family name when it does.
//
// This is the authoritative check — far better than sniffing the executable
// path for "WindowsApps", because a packaged app's children can live anywhere.
func packageIdentity() (bool, string) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetCurrentPackageFamilyName")
	if err := proc.Find(); err != nil {
		return false, "" // pre-Windows 8; no package model at all
	}

	var length uint32
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(&length)), 0)
	if r == appmodelErrorNoPackage {
		return false, ""
	}
	if length == 0 {
		return false, ""
	}

	buf := make([]uint16, length)
	r, _, _ = proc.Call(uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		// Identity exists (the first call did not say otherwise) but the name
		// could not be read; report the fact without a name.
		return true, ""
	}
	return true, strings.TrimRight(syscall.UTF16ToString(buf), "\x00")
}
