package postgres

import (
	"context"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) TrendSessionVersions(ctx context.Context, f db.AnalyticsFilter) (map[string]string, error) {
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithDate(f.TrendsSessionFilter(), "", pb, false, "s.id", s.sessionDialect())
	return db.ScanTrendVersions(s.pg.QueryContext(ctx, `SELECT s.id, `+frustrationSessionVersion("s."+s.trendsVersion)+` FROM sessions s WHERE `+where, pb.args...))
}

// pgTrendSessionWindow selects sessions that start or have a message in the
// padded window. The window bounds are inlined as literals. pgx caches
// prepared statements per connection, and from the sixth run PostgreSQL may
// switch to a generic plan that cannot see how narrow the window is.
func pgTrendSessionWindow(f db.AnalyticsFilter) string {
	sessionWindow := pgAnalyticsMessageWindowLiteral(f, pgDateCol)
	if sessionWindow == "" {
		return ""
	}
	messageWindow := pgAnalyticsMessageWindowLiteral(f, "wm.timestamp")
	return "(" + sessionWindow + " OR EXISTS (SELECT 1 FROM messages wm WHERE wm.session_id = sessions.id AND wm.timestamp IS NOT NULL AND " + messageWindow + "))"
}

func (s *Store) TrendWindowSessions(ctx context.Context, f db.AnalyticsFilter, ids []string) ([]string, error) {
	query := "SELECT id FROM sessions WHERE id = ANY($1)"
	if window := pgTrendSessionWindow(f); window != "" {
		query += " AND " + window
	}
	return db.ScanTrendIDs(s.pg.QueryContext(ctx, query, ids))
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
