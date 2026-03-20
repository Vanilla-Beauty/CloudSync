package apiserver

import (
	"context"
	"net"
	"net/http"
	"os"
	"runtime"

	"github.com/cloudsync/cloudsync/internal/ipc"
	"go.uber.org/zap"
)

// Server serves the REST API over a Unix Domain Socket.
type Server struct {
	mm      MountManagerAPI
	logger  *zap.Logger
	server  *http.Server
	listener net.Listener
	version string
}

// NewServer creates a Server with the given mount manager.
// version is the build-time version string injected via ldflags.
func NewServer(mm MountManagerAPI, logger *zap.Logger, version string) *Server {
	return &Server{mm: mm, logger: logger, version: version}
}

// Start removes any stale socket, listens, and serves in a goroutine.
func (s *Server) Start(socketPath string) error {
	// On Unix, clean up stale socket from previous crash.
	// On Windows (Named Pipe) os.Remove is a no-op on a non-existent pipe.
	if runtime.GOOS != "windows" {
		_ = os.Remove(socketPath)
	}

	ln, err := ipc.Listen(socketPath)
	if err != nil {
		return err
	}
	s.listener = ln

	mux := http.NewServeMux()
	h := &handlers{mm: s.mm, logger: s.logger, version: s.version}
	mux.HandleFunc("/status", h.status)
	mux.HandleFunc("/mounts", h.mounts)
	mux.HandleFunc("/mounts/sync", h.syncMount)
	mux.HandleFunc("/objects/delete", h.deleteObjects)

	s.server = &http.Server{Handler: mux}
	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Warn("API server error", zap.Error(err))
		}
	}()
	return nil
}

// Stop shuts down the HTTP server and removes the socket file.
func (s *Server) Stop() {
	if s.server != nil {
		_ = s.server.Shutdown(context.Background())
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
}
