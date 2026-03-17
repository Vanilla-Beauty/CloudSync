//go:build windows

package ipc

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

// Listen opens a Windows Named Pipe at the given path.
// On Windows, path is a named pipe path such as \\.\pipe\cloudsyncd.
//
// The security descriptor grants read/write access to Everyone (WD) so that
// the daemon can run as a Windows service (Session 0 / SYSTEM) while the CLI
// client runs in the interactive user session.  Without this, the default ACL
// only allows the creating user/session to connect.
func Listen(path string) (net.Listener, error) {
	cfg := &winio.PipeConfig{
		// SDDL: grant FILE_ALL_ACCESS to SYSTEM (SY), Administrators (BA),
		// and the interactive user (IU) — avoids the overly-broad "Everyone".
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;IU)",
	}
	return winio.ListenPipe(path, cfg)
}

// Dial connects to a Windows Named Pipe at the given path.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, path)
}
