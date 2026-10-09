package duckdb

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/readbase"

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
	loc := readbase.AnalyticsLocation(f.Timezone)
	bySession := make(db.MessageScope, len(unique))
	emit := func(m db.ScopedMessage) {
		bySession[m.SessionID] = append(bySession[m.SessionID], m)
	}

	if err := duckQueryChunked(unique, func(chunk []string) error {
		reducer := db.NewScopeReducer(flt, emit)
		ph, args := db.InPlaceholders(chunk)
		rows, err := s.queryContext(ctx, readbase.AnalyticsCandidateMessagesSQL(ph, includeContent),
			args...,
		)
		if err != nil {
			return fmt.Errorf("querying duckdb analytics candidate messages: %w", err)
		}
		defer rows.Close()

		return readbase.ScanAnalyticsMessageScope(rows, "duckdb", formatDBTime, loc, reducer)
	}); err != nil {
		return nil, err
	}

	return bySession, nil
}
