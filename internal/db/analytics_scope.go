package db

import (
	"context"
	"fmt"
	"strings"
)

// MessageScopeFilter adapts analytics model and time filters for message reduction.
func (f AnalyticsFilter) MessageScopeFilter() ScopeFilter {
	models := make(map[string]struct{})
	for _, m := range CSVFilterValues(f.Model) {
		models[m] = struct{}{}
	}
	return ScopeFilter{
		Models:    models,
		DayOfWeek: f.DayOfWeek,
		Hour:      f.Hour,
	}
}

// MessageScope groups model-matched messages by session. Nil means no model filter.
type MessageScope map[string][]ScopedMessage

// resolveAnalyticsMessageScope streams candidate messages for sessionIDs and
// reduces them to the model/time-matched set. It returns nil when no model
// filter is set, signalling the caller to keep its session-grain path.
// includeContent omits the (expensive) content column for count-only panels.
func (db *DB) resolveAnalyticsMessageScope(
	ctx context.Context,
	sessionIDs []string,
	f AnalyticsFilter,
	includeContent bool,
) (MessageScope, error) {
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

	flt := f.MessageScopeFilter()
	loc := f.location()
	bySession := make(MessageScope, len(unique))
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
			SELECT session_id, ordinal, role, COALESCE(source_subtype, ''), is_system, COALESCE(model, ''),
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
				sessionID, role, sourceSubtype, model, ts, content string
				ordinal, outputTokens, contentLength               int
				isSystem, hasThinking, hasToolUse, hasOutputTokens bool
			)
			if err := rows.Scan(
				&sessionID, &ordinal, &role, &sourceSubtype, &isSystem, &model,
				&hasThinking, &hasToolUse, &ts, &outputTokens,
				&hasOutputTokens, &contentLength, &content,
			); err != nil {
				return fmt.Errorf("scanning analytics candidate message: %w", err)
			}
			parsed, has := LocalTime(ts, loc)
			if err := reducer.Push(MessageInput{
				SessionID:       sessionID,
				Ordinal:         ordinal,
				Role:            role,
				SourceSubtype:   sourceSubtype,
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

	return bySession, nil
}

// StatsBySession aggregates matched rows per session.
func (s MessageScope) StatsBySession() map[string]MessageStats {
	out := make(map[string]MessageStats, len(s))
	for id, rows := range s {
		out[id] = ScopeStats(rows)
	}
	return out
}

// TimingBySession projects matched rows into the velocity timing view.
func (s MessageScope) TimingBySession() map[string][]TimingMessage {
	out := make(map[string][]TimingMessage, len(s))
	for id, rows := range s {
		out[id] = ScopeTiming(rows)
	}
	return out
}
