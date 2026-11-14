package readbase

import (
	"context"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

type trendPartsSource struct{ AnalyticsBackend }

func (s trendPartsSource) TrendWindowSessions(ctx context.Context, f db.AnalyticsFilter, ids []string) ([]string, error) {
	var selected []string
	err := s.VisitSessionIDSets(ctx, ids, func(ctx context.Context, set string, args []any) error {
		window, windowArgs := s.TrendSessionWindow(f, set, args)
		found, err := db.ScanTrendIDs(s.QueryContext(ctx, "SELECT s.id FROM sessions s WHERE s.id IN "+set+window, slices.Concat(args, windowArgs)...))
		selected = append(selected, found...)
		return err
	})
	return selected, err
}

func (s trendPartsSource) VisitTrendMessages(ctx context.Context, f db.AnalyticsFilter, ids []string, visit func(string, string, time.Time)) error {
	loc := analyticsLocation(f.Timezone)
	return s.VisitSessionIDSets(ctx, ids, func(ctx context.Context, set string, args []any) error {
		query, args := s.TrendMessagesSQL(set, args)
		rows, err := s.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, content string
			var value any
			if err := rows.Scan(&id, &content, &value); err != nil {
				return err
			}
			if local, ok := trendLocalTime(s.AnalyticsBackend, value, loc); ok {
				visit(id, content, local)
			}
		}
		return rows.Err()
	})
}

func trendLocalTime(backend AnalyticsBackend, value any, loc *time.Location) (time.Time, bool) {
	switch t := value.(type) {
	case time.Time:
		return t.In(loc), true
	case *time.Time:
		if t != nil {
			return t.In(loc), true
		}
		return time.Time{}, false
	default:
		return analyticsLocalTime(backend.FormatTime(value), loc)
	}
}
