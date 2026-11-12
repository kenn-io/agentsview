package server

import (
	"context"
	"os"

	"github.com/danielgtaylor/huma/v2"

	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type PingInfo struct {
	OK                           bool              `json:"ok"`
	Healthy                      bool              `json:"healthy"`
	Service                      string            `json:"service,omitempty"`
	Version                      string            `json:"version,omitempty"`
	PID                          int               `json:"pid,omitempty"`
	Sync                         *syncpkg.Progress `json:"sync,omitempty"`
	ProjectIdentityBackfillError string            `json:"project_identity_backfill_error,omitempty"`
}

func (s *Server) registerHealthRoutes() {
	group := huma.NewGroup(s.api, "/api")
	configureRouteGroup(group, "Health")

	s.get(group, "/ping", "Ping daemon", s.humaPing)
}

func (s *Server) humaPing(
	_ context.Context,
	_ *emptyInput,
) (*jsonOutput[PingInfo], error) {
	healthy := true
	var progress *syncpkg.Progress
	if engine := s.syncStatusEngine(); engine != nil {
		if current, ok := engine.CurrentProgress(); ok {
			progress = &current
			healthy = !current.Stalled
		}
	}
	var backfillError string
	if s.projectIdentityBackfillFailure != nil {
		backfillError = s.projectIdentityBackfillFailure()
	}
	return &jsonOutput[PingInfo]{
		Body: PingInfo{
			OK:                           true,
			Healthy:                      healthy,
			Service:                      daemonService,
			Version:                      s.version.Version,
			PID:                          os.Getpid(),
			Sync:                         progress,
			ProjectIdentityBackfillError: backfillError,
		},
	}, nil
}
