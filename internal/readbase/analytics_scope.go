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

	unique := db.UniqueAnalyticsIDs(sessionIDs)

	flt := f.MessageScopeFilter()
	loc := analyticsLocation(f.Timezone)
	bySession := make(db.MessageScope, len(unique))
	if len(unique) == 0 {
		return bySession, nil
	}
	emit := func(m db.ScopedMessage) {
		bySession[m.SessionID] = append(bySession[m.SessionID], m)
	}

	if err := s.backend.VisitSessionIDSets(ctx, unique, func(ctx context.Context, ph string, args []any) error {
		reducer := db.NewScopeReducer(flt, emit)
		query := analyticsCandidateMessagesSQL(ph, includeContent)
		rows, err := s.backend.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("querying %s analytics candidate messages: %w", s.name, err)
		}
		defer rows.Close()

		return scanAnalyticsMessageScope(rows, s.name, s.backend.FormatTime, loc, reducer)
	}); err != nil {
		return nil, err
	}

	return bySession, nil
}
