package db

import (
	"context"
	"fmt"
	"strings"
)

// messageScopeFilter adapts the model/day/hour parts of an AnalyticsFilter into
// the pure ScopeFilter.
func (f AnalyticsFilter) messageScopeFilter() ScopeFilter {
	models := make(map[string]struct{})
	for _, m := range csvFilterValues(f.Model) {
		models[m] = struct{}{}
	}
	return ScopeFilter{
		Models:    models,
		DayOfWeek: f.DayOfWeek,
		Hour:      f.Hour,
	}
}

// messageScope holds the matched messages for a model-filtered analytics
// request, grouped by session. It is a pure value; all DB work happens during
// resolution.
type messageScope struct {
	bySession map[string][]ScopedMessage
}

// resolveAnalyticsMessageScope streams candidate messages for sessionIDs and
// reduces them to the model/time-matched set. It returns nil when no model
// filter is set, signalling the caller to keep its session-grain path.
// includeContent omits the (expensive) content column for count-only panels.
func (db *DB) resolveAnalyticsMessageScope(
	ctx context.Context,
	sessionIDs []string,
	f AnalyticsFilter,
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

	flt := f.messageScopeFilter()
	loc := f.location()
	bySession := make(map[string][]ScopedMessage, len(unique))
	emit := func(m ScopedMessage) {
		bySession[m.SessionID] = append(bySession[m.SessionID], m)
	}

	contentExpr := "''"
	if includeContent {
		contentExpr = "COALESCE(content, '')"
	}

	if err := queryChunked(unique, func(chunk []string) error {
		reducer := NewScopeReducer(flt, emit)
		ph, args := inPlaceholders(chunk)
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT session_id, ordinal, role, is_system, COALESCE(model, ''),
				has_thinking, has_tool_use, COALESCE(timestamp, ''),
				output_tokens, has_output_tokens, content_length, `+contentExpr+`
			FROM messages
			WHERE session_id IN `+ph+`
			ORDER BY session_id, ordinal`,
			args...,
		)
		if err != nil {
			return fmt.Errorf("querying analytics candidate messages: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				sessionID, role, model, ts, content                string
				ordinal, outputTokens, contentLength               int
				isSystem, hasThinking, hasToolUse, hasOutputTokens bool
			)
			if err := rows.Scan(
				&sessionID, &ordinal, &role, &isSystem, &model,
				&hasThinking, &hasToolUse, &ts, &outputTokens,
				&hasOutputTokens, &contentLength, &content,
			); err != nil {
				return fmt.Errorf("scanning analytics candidate message: %w", err)
			}
			parsed, has := localTime(ts, loc)
			if err := reducer.Push(MessageInput{
				SessionID:       sessionID,
				Ordinal:         ordinal,
				Role:            role,
				Model:           model,
				IsSystem:        isSystem,
				Timestamp:       ts,
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
			return fmt.Errorf("iterating analytics candidate messages: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &messageScope{bySession: bySession}, nil
}

// MessagesBySession returns the matched rows per session.
func (s *messageScope) MessagesBySession() map[string][]ScopedMessage {
	return s.bySession
}

// StatsBySession aggregates matched rows per session.
func (s *messageScope) StatsBySession() map[string]MessageStats {
	out := make(map[string]MessageStats, len(s.bySession))
	for id, rows := range s.bySession {
		out[id] = ScopeStats(rows)
	}
	return out
}

// TimingBySession projects matched rows into the velocity timing view.
func (s *messageScope) TimingBySession() map[string][]TimingMessage {
	out := make(map[string][]TimingMessage, len(s.bySession))
	for id, rows := range s.bySession {
		out[id] = ScopeTiming(rows)
	}
	return out
}
