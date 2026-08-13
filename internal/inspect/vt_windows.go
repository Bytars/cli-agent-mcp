//go:build windows

package inspect

import (
	"syscall"
	"unsafe"
)

// enableVirtualTerminalProcessing makes the console interpret ANSI escape
// sequences instead of printing them literally. Windows Terminal has it on
// already; the legacy console does not, and there this is the difference
// between coloured output and a screen full of "<ESC>[36m".
const enableVirtualTerminalProcessing = 0x0004

var (
	consoleDLL         = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = consoleDLL.NewProc("GetConsoleMode")
	procSetConsoleMode = consoleDLL.NewProc("SetConsoleMode")
)

// enableANSI reports whether colour is safe to emit, turning it on when the
// console supports it but has it disabled. It fails closed: anything unexpected
// means plain text, never escape codes the terminal would show verbatim.
func enableANSI() bool {
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil || h == syscall.InvalidHandle {
		return false
	}
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ := procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
