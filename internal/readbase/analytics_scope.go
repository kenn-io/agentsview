package readbase

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// ResolveMessageScope returns nil without a model filter, preserving session-level reads.
func (s *Analytics) ResolveMessageScope(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
	includeContent bool,
) (db.MessageScope, error) {
	if strings.TrimSpace(f.Model) == "" {
		return nil, nil
	}

	unique := uniqueAnalyticsIDs(sessionIDs)

	flt := f.MessageScopeFilter()
	loc := AnalyticsLocation(f.Timezone)
	bySession := make(db.MessageScope, len(unique))
	emit := func(m db.ScopedMessage) {
		bySession[m.SessionID] = append(bySession[m.SessionID], m)
	}

	if err := db.QueryChunkedSize(unique, AnalyticsMaxSQLVars, func(chunk []string) error {
		reducer := db.NewScopeReducer(flt, emit)
		query, args := s.backend.CandidateMessagesSQL(chunk, includeContent)
		rows, err := s.backend.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("querying %s analytics candidate messages: %w", s.name, err)
		}
		defer rows.Close()

		return ScanAnalyticsMessageScope(rows, s.name, s.backend.FormatTime, loc, reducer)
	}); err != nil {
		return nil, err
	}

	return bySession, nil
}
