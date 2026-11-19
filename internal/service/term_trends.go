package service

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// TermTrendsRequest asks for term-frequency counts over time. Each entry in
// Terms is one series; "|" separates variants counted together. It mirrors
// GET /api/v1/trends/terms.
type TermTrendsRequest struct {
	AnalyticsRequest
	Terms       []string `json:"terms"`
	Granularity string   `json:"granularity,omitempty"`
}

// BuildTermTrendsQuery validates req and returns the analytics filter,
// parsed terms, and bucket granularity (default week). Machine is passed
// through unresolved.
func BuildTermTrendsQuery(
	req TermTrendsRequest, now time.Time,
) (db.AnalyticsFilter, []db.TrendTermInput, string, error) {
	f, err := BuildAnalyticsFilter(req.AnalyticsRequest, now)
	if err != nil {
		return db.AnalyticsFilter{}, nil, "", err
	}
	granularity := req.Granularity
	switch granularity {
	case "":
		granularity = "week"
	case "day", "week", "month":
	default:
		return db.AnalyticsFilter{}, nil, "", &AnalyticsInputError{
			Msg: "invalid granularity: use day, week, or month",
		}
	}
	terms, err := db.ParseTrendTerms(req.Terms)
	if err != nil {
		return db.AnalyticsFilter{}, nil, "", &AnalyticsInputError{Msg: err.Error()}
	}
	return f, terms, granularity, nil
}

func (b *directBackend) TermTrends(
	ctx context.Context, req TermTrendsRequest,
) (*db.TrendsTermsResponse, error) {
	f, terms, granularity, err := BuildTermTrendsQuery(req, time.Now())
	if err != nil {
		return nil, err
	}
	f.Machine, err = db.ResolveMachineFilter(ctx, b.db, f.Machine)
	if err != nil {
		return nil, err
	}
	res, err := b.db.GetTrendsTerms(ctx, f, terms, granularity)
	if err != nil {
		return nil, err
	}
	return &res, nil
}
