package server

import (
	"context"

	"go.kenn.io/agentsview/internal/service"
)

func (s *Server) registerRateLimitRoutes() {
	group := newRouteGroup(s.api, "/api/v1/rate-limits", "RateLimits")

	s.get(group, "/current", "Get current rate limits", s.humaRateLimitsCurrent)
	s.get(group, "/history", "Get rate limit history", s.humaRateLimitsHistory)
}

// RateLimitFilterInput is the shared vendor/account/machine filter for
// both rate-limits endpoints. The table is SQLite-only (see
// docs/agents/storage.md) and Codex-only today; Agent takes the same
// comma-separated selection as the shared session filters, and a
// selection that omits the resolved vendor (Vendor, or "codex" when
// unset) matches nothing.
type RateLimitFilterInput struct {
	Vendor    string `query:"vendor" enum:"codex" doc:"Filter by vendor"`
	AccountID string `query:"account_id" doc:"Filter by account id; scopes vendors that have accounts and never excludes an account-less vendor's rows (Codex today)"`
	Machine   string `query:"machine" doc:"Filter by machine (comma-separated)"`
	// Agent is accepted for backward compatibility with the original
	// Codex-only filter name; it behaves like Vendor when Vendor is
	// unset.
	Agent string `query:"agent" doc:"Deprecated alias for vendor (comma-separated)"`
}

type rateLimitsHistoryInput struct {
	RateLimitFilterInput
	LimitID    string `query:"limit_id" doc:"Filter by limit id (e.g. codex)"`
	WindowKind string `query:"window" enum:"primary,secondary" doc:"Filter by rate-limit window kind"`
	Since      string `query:"since" format:"date-time" doc:"Return snapshots observed at or after this RFC3339 timestamp"`
	Until      string `query:"until" format:"date-time" doc:"Return snapshots observed strictly before this RFC3339 timestamp"`
	// MaxPoints bounds the response size for a wide date range: a range
	// with more matching observations than this is downsampled to at
	// most this many points, keeping the most recently observed row per
	// time bucket rather than every observation.
	MaxPoints int `query:"max_points" minimum:"1" maximum:"2000" default:"500" doc:"Maximum points returned; a wider range is downsampled to this many, keeping the most recent observation per time bucket"`
}

func rateLimitFilterRequestFromInput(in RateLimitFilterInput) service.RateLimitFilterRequest {
	return service.RateLimitFilterRequest{
		Vendor:    in.Vendor,
		AccountID: in.AccountID,
		Machine:   in.Machine,
		Agent:     in.Agent,
	}
}

func (s *Server) humaRateLimitsCurrent(
	ctx context.Context,
	in *RateLimitFilterInput,
) (*jsonOutput[[]service.RateLimitWindow], error) {
	rows, err := service.RateLimitCurrent(ctx, s.db, rateLimitFilterRequestFromInput(*in))
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("rate limits current error", err)
	}
	return &jsonOutput[[]service.RateLimitWindow]{Body: rows}, nil
}

func (s *Server) humaRateLimitsHistory(
	ctx context.Context,
	in *rateLimitsHistoryInput,
) (*jsonOutput[[]service.RateLimitWindow], error) {
	rows, err := service.RateLimitHistory(ctx, s.db, service.RateLimitHistoryRequest{
		RateLimitFilterRequest: rateLimitFilterRequestFromInput(in.RateLimitFilterInput),
		LimitID:                in.LimitID,
		WindowKind:             in.WindowKind,
		Since:                  in.Since,
		Until:                  in.Until,
		MaxPoints:              in.MaxPoints,
	})
	if err != nil {
		if handled := handleHumaContextError(err); handled != nil {
			return nil, handled
		}
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("rate limits history error", err)
	}
	return &jsonOutput[[]service.RateLimitWindow]{Body: rows}, nil
}
