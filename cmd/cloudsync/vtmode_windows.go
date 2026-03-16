//go:build windows

package main

import (
	"golang.org/x/sys/windows"
)

// enableVTMode enables ANSI/VT100 escape code processing on Windows 10+.
// This is required for the interactive browser (ls-remote) to display correctly.
// Older Windows versions that do not support VT mode will silently ignore the call.
func enableVTMode() {
	stdout := windows.Handle(uintptr(windows.Stdout))
	var mode uint32
	if err := windows.GetConsoleMode(stdout, &mode); err != nil {
		return
	}
	// ENABLE_VIRTUAL_TERMINAL_PROCESSING = 0x0004
	_ = windows.SetConsoleMode(stdout, mode|0x0004)
}
