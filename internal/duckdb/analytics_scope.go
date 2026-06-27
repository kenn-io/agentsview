package duckdb

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/analyticscope"
	"go.kenn.io/agentsview/internal/db"
)

// messageScopeFilter adapts the model/day/hour parts of an AnalyticsFilter into
// the pure analyticscope.Filter. It is a free function because AnalyticsFilter
// is defined in package db.
func messageScopeFilter(f db.AnalyticsFilter) analyticscope.Filter {
	models := make(map[string]struct{})
	for _, m := range duckAnalyticsCSVValues(f.Model) {
		models[m] = struct{}{}
	}
	return analyticscope.Filter{
		Models:    models,
		DayOfWeek: f.DayOfWeek,
		Hour:      f.Hour,
	}
}

// messageScope holds the matched messages for a model-filtered analytics
// request, grouped by session. It is a pure value; all DB work happens during
// resolution. Its consumers are message stats, velocity timing, and signal
// evidence (which reads message content), mirroring the SQLite/Postgres scopes.
type messageScope struct {
	bySession map[string][]analyticscope.ScopedMessage
}

// resolveAnalyticsMessageScope streams candidate messages for sessionIDs and
// reduces them to the model/time-matched set. It returns nil when no model
// filter is set, signalling the caller to keep its session-grain path.
func (s *Store) resolveAnalyticsMessageScope(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
	includeContent bool,
) (*messageScope, error) {
	if strings.TrimSpace(f.Model) == "" {
		return nil, nil
	}

	seen := make(map[string]struct{}, len(sessionIDs))
	unique := make([]string, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	flt := messageScopeFilter(f)
	loc := analyticsLocation(f.Timezone)
	bySession := make(map[string][]analyticscope.ScopedMessage, len(unique))
	emit := func(m analyticscope.ScopedMessage) {
		bySession[m.SessionID] = append(bySession[m.SessionID], m)
	}

	contentExpr := "''"
	if includeContent {
		contentExpr = "COALESCE(content, '')"
	}

	if err := duckQueryChunked(unique, func(chunk []string) error {
		reducer := analyticscope.NewReducer(flt, emit)
		ph, args := duckInPlaceholders(chunk)
		rows, err := s.duck.QueryContext(ctx, `
			SELECT session_id, ordinal, role, is_system, COALESCE(model, ''),
				has_thinking, has_tool_use, timestamp,
				output_tokens, has_output_tokens, content_length, `+contentExpr+`
			FROM messages
			WHERE session_id IN `+ph+`
			ORDER BY session_id, ordinal`,
			args...,
		)
		if err != nil {
			return fmt.Errorf("querying duckdb analytics candidate messages: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				sessionID, role, model, content                    string
				ordinal, outputTokens, contentLength               int
				isSystem, hasThinking, hasToolUse, hasOutputTokens bool
				ts                                                 any
			)
			if err := rows.Scan(
				&sessionID, &ordinal, &role, &isSystem, &model,
				&hasThinking, &hasToolUse, &ts, &outputTokens,
				&hasOutputTokens, &contentLength, &content,
			); err != nil {
				return fmt.Errorf("scanning duckdb analytics candidate message: %w", err)
			}
			tsStr := formatDBTime(ts)
			parsed, has := duckLocalTime(tsStr, loc)
			if err := reducer.Push(analyticscope.MessageInput{
				SessionID:       sessionID,
				Ordinal:         ordinal,
				Role:            role,
				Model:           model,
				IsSystem:        isSystem,
				Timestamp:       tsStr,
				LocalTime:       parsed,
				HasLocalTime:    has,
				HasThinking:     hasThinking,
				HasToolUse:      hasToolUse,
				OutputTokens:    outputTokens,
				HasOutputTokens: hasOutputTokens,
				ContentLength:   contentLength,
				Content:         content,
			}); err != nil {
				return err
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating duckdb analytics candidate messages: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &messageScope{bySession: bySession}, nil
}

// MessagesBySession returns the matched rows per session.
func (s *messageScope) MessagesBySession() map[string][]analyticscope.ScopedMessage {
	return s.bySession
}

// StatsBySession aggregates matched rows per session.
func (s *messageScope) StatsBySession() map[string]analyticscope.MessageStats {
	out := make(map[string]analyticscope.MessageStats, len(s.bySession))
	for id, rows := range s.bySession {
		out[id] = analyticscope.Stats(rows)
	}
	return out
}

// TimingBySession projects matched rows into the velocity timing view.
func (s *messageScope) TimingBySession() map[string][]analyticscope.TimingMessage {
	out := make(map[string][]analyticscope.TimingMessage, len(s.bySession))
	for id, rows := range s.bySession {
		out[id] = analyticscope.Timing(rows)
	}
	return out
}
