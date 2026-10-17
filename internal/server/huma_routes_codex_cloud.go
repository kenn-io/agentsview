package server

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/codexcloud"
)

// WithCodexCloudStatus serves the codex-cloud source's status rows.
func WithCodexCloudStatus(board *codexcloud.StatusBoard) Option {
	return func(s *Server) {
		s.codexCloudStatus = board
	}
}

// registerCodexCloudRoutes always registers the status route: the source
// can be configured by a config.toml edit without a restart, so the route
// answers an empty list until then.
func (s *Server) registerCodexCloudRoutes() {
	group := huma.NewGroup(s.api, "/api/v1/codex-cloud")
	configureRouteGroup(group, "CodexCloud")
	registerRoute(group,
		http.MethodGet, "/status", "Read codex-cloud import status",
		s.humaCodexCloudStatus, s.humaTimeout(),
	)
}

func (s *Server) humaCodexCloudStatus(
	context.Context, *struct{},
) (*jsonOutput[[]codexcloud.HomeStatus], error) {
	rows := []codexcloud.HomeStatus{}
	if s.codexCloudStatus != nil {
		rows = s.codexCloudStatus.Snapshot()
	}
	return &jsonOutput[[]codexcloud.HomeStatus]{Body: rows}, nil
}
