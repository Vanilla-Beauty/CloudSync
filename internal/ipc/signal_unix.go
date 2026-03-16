//go:build !windows

package ipc

import (
	"os"
	"syscall"
)

// Terminate sends SIGTERM to the process, requesting a graceful shutdown.
func Terminate(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}
