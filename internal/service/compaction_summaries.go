package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// RooCode and legacy Kilo mark context-management events as boundaries.
var compactionExcludedAgents = [...]string{string(parser.AgentRooCode), string(parser.AgentKiloLegacy)}

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
	Summaries  []CompactionSummary   `json:"summaries"`
	Ambiguous  []CompactionAmbiguity `json:"ambiguous,omitempty"`
	NextCursor string                `json:"next_cursor,omitempty"`
}

type CompactionAmbiguity struct {
	SessionID string `json:"session_id"`
	Ordinal   int    `json:"ordinal"`
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
	limit = min(limit, MaxCompactionSessionLimit)
	if checker, ok := svc.(interface{ CheckCompactionSummaries(context.Context) error }); ok {
		if err := checker.CheckCompactionSummaries(ctx); err != nil {
			return out, err
		}
	}
	var sessions []db.Session
	if req.SessionID != "" {
		detail, err := svc.Get(ctx, req.SessionID)
		if err != nil {
			return out, err
		}
		if detail == nil || slices.Contains(compactionExcludedAgents[:], detail.Agent) {
			return out, nil
		}
		sessions = append(sessions, detail.Session)
	} else {
		page, err := svc.List(ctx, ListFilter{ActiveSince: req.ActiveSince, Cursor: req.Cursor, Limit: limit, IncludeOneShot: true, OneShotFilterSet: true, IncludeAutomated: true, AutomatedFilterSet: true, IncludeChildren: true, Flat: true})
		if err != nil {
			return out, err
		}
		out.NextCursor = page.NextCursor
		for _, session := range page.Sessions {
			if !slices.Contains(compactionExcludedAgents[:], session.Agent) {
				sessions = append(sessions, session)
			}
		}
	}
	for _, session := range sessions {
		from := 0
		for {
			page, err := svc.Messages(ctx, session.ID, MessageFilter{From: &from, Direction: "asc", Limit: db.MaxMessageLimit, CompactBoundaryOnly: true})
			if err != nil {
				return out, err
			}
			for _, m := range page.Messages {
				appendCompactionSummary(&out, session, m)
			}
			if len(page.Messages) < db.MaxMessageLimit {
				break
			}
			from = page.Messages[len(page.Messages)-1].Ordinal + 1
		}
	}
	return out, nil
}

func appendCompactionSummary(out *CompactionSummariesResult, session db.Session, m db.Message) {
	if !m.IsCompactBoundary || strings.TrimSpace(m.Content) == "" {
		return
	}
	if session.Agent == string(parser.AgentEvener) {
		if m.SourceType == parser.EvenerEmptyCompactionSourceType {
			return
		}
		if (m.SourceSubtype == "SUMMARY" || m.SourceSubtype == "CHECKPOINT") && m.Content == "["+m.SourceSubtype+"]" && m.SourceType != parser.EvenerLiteralCompactionSourceType {
			out.Ambiguous = append(out.Ambiguous, CompactionAmbiguity{SessionID: session.ID, Ordinal: m.Ordinal})
			return
		}
	}
	out.Summaries = append(out.Summaries, CompactionSummary{SessionID: session.ID, Ordinal: m.Ordinal, Timestamp: m.Timestamp, Content: m.Content})
}
