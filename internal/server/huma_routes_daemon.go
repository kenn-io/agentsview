package server

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

func (s *Server) registerDaemonRoutes() {
	group := huma.NewGroup(s.api, "/api/v1/daemon")
	configureRouteGroup(group, "Daemon")
	s.post(group, "/persist", "Keep the daemon running until stopped", s.humaPersistDaemon)
}

func (s *Server) humaPersistDaemon(_ context.Context, _ *emptyInput) (*jsonOutput[map[string]bool], error) {
	if s.persistDaemon == nil {
		return nil, apiError(http.StatusNotImplemented, "daemon lifetime is unavailable on this server")
	}
	s.persistDaemonMu.Lock()
	defer s.persistDaemonMu.Unlock()
	if err := s.persistDaemon(); err != nil {
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	if !s.idle.DisableShutdown() {
		return nil, apiError(http.StatusServiceUnavailable, "server is shutting down")
	}
	return &jsonOutput[map[string]bool]{Body: map[string]bool{"persistent": true}}, nil
}
