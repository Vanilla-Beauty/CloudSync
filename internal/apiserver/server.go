package apiserver

import (
	"context"
	"net"
	"net/http"
	"os"

	"go.uber.org/zap"
)

// Server serves the REST API over a Unix Domain Socket.
type Server struct {
	mm       MountManagerAPI
	logger   *zap.Logger
	server   *http.Server
	listener net.Listener
}

// NewServer creates a Server with the given mount manager.
func NewServer(mm MountManagerAPI, logger *zap.Logger) *Server {
	return &Server{mm: mm, logger: logger}
}

// Start removes any stale socket, listens, and serves in a goroutine.
func (s *Server) Start(socketPath string) error {
	// Clean up stale socket from previous crash
	_ = os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	s.listener = ln

	mux := http.NewServeMux()
	h := &handlers{mm: s.mm, logger: s.logger}
	mux.HandleFunc("/status", h.status)
	mux.HandleFunc("/mounts", h.mounts)
	mux.HandleFunc("/mounts/pause", h.pauseMount)
	mux.HandleFunc("/mounts/resume", h.resumeMount)

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
