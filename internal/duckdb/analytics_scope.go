package duckdb

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// resolveAnalyticsMessageScope streams candidate messages for sessionIDs and
// reduces them to the model/time-matched set. It returns nil when no model
// filter is set, signalling the caller to keep its session-grain path.
func (s *Store) resolveAnalyticsMessageScope(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
	includeContent bool,
) (db.MessageScope, error) {
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
	loc := analyticsLocation(f.Timezone)
	bySession := make(db.MessageScope, len(unique))
	emit := func(m db.ScopedMessage) {
		bySession[m.SessionID] = append(bySession[m.SessionID], m)
	}

	contentExpr := "''"
	if includeContent {
		contentExpr = "COALESCE(content, '')"
	}

	if err := duckQueryChunked(unique, func(chunk []string) error {
		reducer := db.NewScopeReducer(flt, emit)
		ph, args := duckInPlaceholders(chunk)
		rows, err := s.queryContext(ctx, `
			SELECT session_id, ordinal, role, COALESCE(source_subtype, ''), is_system, COALESCE(model, ''),
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
				sessionID, role, sourceSubtype, model, content     string
				ordinal, outputTokens, contentLength               int
				isSystem, hasThinking, hasToolUse, hasOutputTokens bool
				ts                                                 any
			)
			if err := rows.Scan(
				&sessionID, &ordinal, &role, &sourceSubtype, &isSystem, &model,
				&hasThinking, &hasToolUse, &ts, &outputTokens,
				&hasOutputTokens, &contentLength, &content,
			); err != nil {
				return fmt.Errorf("scanning duckdb analytics candidate message: %w", err)
			}
			tsStr := formatDBTime(ts)
			parsed, has := duckLocalTime(tsStr, loc)
			if err := reducer.Push(db.MessageInput{
				SessionID:       sessionID,
				Ordinal:         ordinal,
				Role:            role,
				SourceSubtype:   sourceSubtype,
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

	return bySession, nil
}
