//go:build !windows

package main

// enableVTMode is a no-op on Unix; ANSI codes are always supported.
func enableVTMode() {}
