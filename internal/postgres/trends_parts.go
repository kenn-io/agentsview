package postgres

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// trendSelection selects the sessions a Trends read walks.
func (s *Store) trendSelection(f db.AnalyticsFilter) (string, []any) {
	sf := f
	sf.From, sf.To, sf.Model = "", "", ""
	sf.DayOfWeek, sf.Hour = nil, nil
	pb := &paramBuilder{}
	preds := []string{buildAnalyticsWhereWithDate(sf, "", pb, false, "s.id", s.sessionDialect())}
	// The window bounds are inlined as literals. pgx caches prepared
	// statements per connection, and from the sixth run PostgreSQL may switch
	// to a generic plan that cannot see how narrow the window is.
	if sessionWindow := pgAnalyticsMessageWindowLiteral(f, pgDateCol); sessionWindow != "" {
		messageWindow := pgAnalyticsMessageWindowLiteral(f, "wm.timestamp")
		preds = append(preds, "s.id IN (SELECT sessions.id FROM sessions WHERE ("+sessionWindow+" OR EXISTS (SELECT 1 FROM messages wm WHERE wm.session_id = sessions.id AND wm.timestamp IS NOT NULL AND "+messageWindow+")))")
	}
	return strings.Join(preds, " AND "), pb.args
}

func (s *Store) TrendSessionVersions(ctx context.Context, f db.AnalyticsFilter) (map[string]string, error) {
	where, args := s.trendSelection(f)
	return db.ScanTrendVersions(s.pg.QueryContext(ctx, `SELECT s.id, `+frustrationSessionVersion("s."+s.trendsVersionColumn)+` FROM sessions s WHERE `+where, args...))
}

func (s *Store) VisitTrendMessages(ctx context.Context, f db.AnalyticsFilter, ids []string, visit func(string, string, time.Time)) error {
	rows, err := s.pg.QueryContext(ctx, `SELECT m.session_id, m.content, COALESCE(m.timestamp, s.started_at, s.created_at) FROM messages m JOIN sessions s ON s.id = m.session_id WHERE m.session_id = ANY($1) AND m.role IN ('user', 'assistant') AND m.is_system = FALSE AND `+db.PostgresSystemPrefixSQL("m.content", "m.role"), ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	loc := db.LoadLocationOr(f.Timezone, time.UTC)
	for rows.Next() {
		var id, content string
		var ts *time.Time
		if err := rows.Scan(&id, &content, &ts); err != nil {
			return err
		}
		if ts != nil {
			visit(id, content, ts.In(loc))
		}
	}
	return rows.Err()
}
