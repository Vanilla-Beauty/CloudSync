//go:build !windows

package ipc

import (
	"context"
	"net"
)

// Listen opens a Unix Domain Socket at the given path.
func Listen(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}

// Dial connects to a Unix Domain Socket at the given path.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
