package readbase

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

type trendPartsSource struct{ backend AnalyticsBackend }

func (s trendPartsSource) TrendSessionVersions(ctx context.Context, f db.AnalyticsFilter) (map[string]string, error) {
	return s.backend.TrendSessionVersions(ctx, f)
}
func (s trendPartsSource) VisitTrendMessages(ctx context.Context, f db.AnalyticsFilter, ids []string, visit func(string, string, time.Time)) error {
	loc := analyticsLocation(f.Timezone)
	return s.backend.VisitSessionIDSets(ctx, ids, func(ctx context.Context, set string, args []any) error {
		query, args := s.backend.TrendMessagesSQL(set, args)
		rows, err := s.backend.QueryContext(ctx, query, args...)
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
			var local time.Time
			var ok bool
			switch t := value.(type) {
			case time.Time:
				local, ok = t.In(loc), true
			case *time.Time:
				if t != nil {
					local, ok = t.In(loc), true
				}
			default:
				local, ok = analyticsLocalTime(s.backend.FormatTime(value), loc)
			}
			if ok {
				visit(id, content, local)
			}
		}
		return rows.Err()
	})
}
