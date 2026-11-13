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
	s.post(group, "/restart", "Restart the daemon with saved settings", s.humaRestartDaemon)
}

func (s *Server) restartSupported() bool {
	return s.restartDaemon != nil && s.db != nil && !s.db.ReadOnly()
}

func (s *Server) humaRestartDaemon(ctx context.Context, _ *emptyInput) (*jsonOutput[map[string]bool], error) {
	if !s.restartSupported() {
		return nil, apiError(http.StatusNotImplemented, "Restart AgentsView the way you started it, then reload this page.")
	}
	// The replacement is built from this snapshot. Holding settingsApplyMu
	// until the restart is accepted keeps a concurrent save, such as turning
	// on authentication, from landing after the snapshot and being dropped.
	s.settingsApplyMu.Lock()
	defer s.settingsApplyMu.Unlock()
	s.mu.RLock()
	current := s.cfg
	s.mu.RUnlock()
	if err := s.restartDaemon(ctx, current); err != nil {
		return nil, apiError(http.StatusInternalServerError, err.Error())
	}
	s.restartAccepted = true
	return &jsonOutput[map[string]bool]{Body: map[string]bool{"restarting": true}}, nil
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
