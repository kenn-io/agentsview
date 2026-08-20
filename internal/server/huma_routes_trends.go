package server

import (
	"context"
	"net/http"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Server) registerTrendsRoutes() {
	group := newRouteGroup(s.api, "/api/v1/trends", "Trends")

	s.get(group, "/terms", "Get trend terms", s.humaTrendsTerms)
}

type trendsTermsInput struct {
	AnalyticsFilterInput
	Term        []string             `query:"term,explode" doc:"Terms to trend"`
	Granularity analyticsGranularity `query:"granularity" enum:"day,week,month" default:"week" doc:"Time bucket granularity"`
}

func (s *Server) humaTrendsTerms(
	ctx context.Context,
	in *trendsTermsInput,
) (*jsonOutput[db.TrendsTermsResponse], error) {
	f, err := analyticsFilterFromInput(in.AnalyticsFilterInput)
	if err != nil {
		return nil, err
	}
	terms, err := db.ParseTrendTerms(in.Term)
	if err != nil {
		return nil, apiError(http.StatusBadRequest, err.Error())
	}
	result, err := s.db.GetTrendsTerms(ctx, f, terms, string(in.Granularity))
	if err != nil {
		return nil, internalError("trends terms error", err)
	}
	return &jsonOutput[db.TrendsTermsResponse]{Body: result}, nil
}
