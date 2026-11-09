package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/atomicfile"
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
	err := s.persistDaemon()
	if err == nil || errors.Is(err, atomicfile.ErrPublished) {
		s.idle.DisableShutdown()
	}
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	return &jsonOutput[map[string]bool]{Body: map[string]bool{"persistent": true}}, nil
}
