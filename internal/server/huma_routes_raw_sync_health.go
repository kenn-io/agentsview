package server

import (
	"context"
	"net/http"

	"go.kenn.io/agentsview/internal/rawsync"

	"github.com/danielgtaylor/huma/v2"
)

func (s *Server) registerRawSyncHealthRoute(group *huma.Group) {
	if s.rawSyncJobHealth == nil && !s.rawSyncSchemaOnly {
		return
	}
	registerRoute(
		group, http.MethodGet, "/health", "Report raw parse-job health",
		s.humaRawSyncJobHealth, s.humaTimeout(),
	)
}

type rawSyncJobHealthInput struct {
	Authorization     string `header:"Authorization"`
	MaxAttempts       int32  `query:"max_attempts" required:"true" minimum:"1" maximum:"2147483647"`
	StaleAfterSeconds int32  `query:"stale_after_seconds" required:"true" minimum:"1" maximum:"2147483647"`
}

func (s *Server) humaRawSyncJobHealth(
	ctx context.Context,
	in *rawSyncJobHealthInput,
) (*jsonOutput[rawsync.JobHealthReport], error) {
	identity, err := rawSyncIdentityFromContext(ctx)
	if err != nil {
		return nil, err
	}
	query := rawsync.JobHealthQuery{
		MaxAttempts:       in.MaxAttempts,
		StaleAfterSeconds: in.StaleAfterSeconds,
	}
	if err := query.Validate(); err != nil {
		return nil, rawSyncHTTPError(err)
	}
	report, err := s.rawSyncJobHealth.RawJobHealth(ctx, identity, query)
	if err != nil {
		return nil, rawSyncHTTPError(err)
	}
	return &jsonOutput[rawsync.JobHealthReport]{Body: report}, nil
}
