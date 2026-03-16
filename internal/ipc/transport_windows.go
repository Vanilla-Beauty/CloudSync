//go:build windows

package ipc

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

// Listen opens a Windows Named Pipe at the given path.
// On Windows, path is a named pipe path such as \\.\pipe\cloudsyncd.
func Listen(path string) (net.Listener, error) {
	return winio.ListenPipe(path, nil)
}

// Dial connects to a Windows Named Pipe at the given path.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, path)
}
