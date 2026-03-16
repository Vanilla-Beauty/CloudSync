//go:build windows

package ipc

import "os"

// Terminate kills the process on Windows, which does not support SIGTERM.
// The daemon's service manager (kardianos/service) handles graceful shutdown
// via the SCM stop signal, so direct kill is only the fallback path.
func Terminate(proc *os.Process) error {
	return proc.Kill()
}
