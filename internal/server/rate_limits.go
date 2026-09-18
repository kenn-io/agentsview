package server

import (
	"context"
	"net/http"

	"go.kenn.io/agentsview/internal/db"
)

type rateLimitsInput struct {
	Machine string `query:"machine" doc:"Filter by machine"`
	Since   int64  `query:"since" minimum:"0" maximum:"9223372036" doc:"Inclusive history start, Unix seconds"`
	Until   int64  `query:"until" minimum:"0" maximum:"9223372036" doc:"Exclusive history end, Unix seconds"`
}

func (s *Server) humaRateLimits(ctx context.Context, in *rateLimitsInput) (*jsonOutput[[]db.RateLimitSeries], error) {
	if in.Until != 0 && in.Since >= in.Until {
		return nil, apiError(http.StatusBadRequest, "history start must precede its end")
	}
	local, ok := s.db.(*db.DB)
	if !ok {
		return nil, apiError(http.StatusNotImplemented, "not available in remote mode")
	}
	machine, err := db.ResolveMachineFilter(ctx, s.db, in.Machine)
	if err != nil {
		return nil, serverError(err)
	}
	series, err := local.RateLimits(ctx, machine, in.Since, in.Until)
	if err != nil {
		return nil, apiError(http.StatusInternalServerError, "reading rate limits: "+err.Error())
	}
	return &jsonOutput[[]db.RateLimitSeries]{Body: series}, nil
}
