package service

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

const (
	DefaultCompactionSessionLimit = 20
	MaxCompactionSessionLimit     = 100
)

type CompactionSummariesRequest struct {
	SessionID   string `json:"session_id,omitempty"`
	ActiveSince string `json:"active_since,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

type CompactionSummary struct {
	SessionID string `json:"session_id"`
	Ordinal   int    `json:"ordinal"`
	Timestamp string `json:"timestamp"`
	Content   string `json:"content"`
}

type CompactionSummariesResult struct {
	Summaries  []CompactionSummary `json:"summaries"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

func CompactionSummaries(ctx context.Context, svc SessionService, req CompactionSummariesRequest) (CompactionSummariesResult, error) {
	out := CompactionSummariesResult{Summaries: []CompactionSummary{}}
	if (req.SessionID == "") == (req.ActiveSince == "") {
		return out, errors.New("provide exactly one of session_id or active_since")
	}
	if req.SessionID != "" && req.Cursor != "" {
		return out, errors.New("cursor requires active_since")
	}
	limit := req.Limit
	if limit < 0 {
		return out, errors.New("limit must be 0 or more")
	}
	if limit == 0 {
		limit = DefaultCompactionSessionLimit
	}
	if limit > MaxCompactionSessionLimit {
		limit = MaxCompactionSessionLimit
	}
	if checker, ok := svc.(interface{ CheckCompactionSummaries(context.Context) error }); ok {
		if err := checker.CheckCompactionSummaries(ctx); err != nil {
			return out, err
		}
	}
	ids := []string{req.SessionID}
	if req.SessionID != "" {
		detail, err := svc.Get(ctx, req.SessionID)
		if err != nil {
			return out, err
		}
		if detail == nil || db.IsCompactionEventAgent(detail.Agent) {
			return out, nil
		}
	}
	if req.ActiveSince != "" {
		page, err := svc.List(ctx, ListFilter{ActiveSince: req.ActiveSince, Cursor: req.Cursor, Limit: limit, IncludeOneShot: true, OneShotFilterSet: true, IncludeAutomated: true, AutomatedFilterSet: true, IncludeChildren: true, HasCompactionSummary: true})
		if err != nil {
			return out, err
		}
		out.NextCursor = page.NextCursor
		ids = ids[:0]
		for _, session := range page.Sessions {
			if !db.IsCompactionEventAgent(session.Agent) {
				ids = append(ids, session.ID)
			}
		}
	}
	for _, id := range ids {
		from := 0
		for {
			page, err := svc.Messages(ctx, id, MessageFilter{From: &from, Direction: "asc", Limit: db.MaxMessageLimit, CompactBoundaryOnly: true})
			if err != nil {
				return out, err
			}
			for _, m := range page.Messages {
				if m.IsCompactBoundary && strings.TrimSpace(m.Content) != "" && !db.IsCompactionPlaceholder(m.SourceType) {
					out.Summaries = append(out.Summaries, CompactionSummary{SessionID: id, Ordinal: m.Ordinal, Timestamp: m.Timestamp, Content: m.Content})
				}
			}
			if len(page.Messages) < db.MaxMessageLimit {
				break
			}
			from = page.Messages[len(page.Messages)-1].Ordinal + 1
		}
	}
	return out, nil
}
