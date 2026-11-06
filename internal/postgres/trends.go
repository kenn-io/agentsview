package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) GetTrendsTerms(
	ctx context.Context,
	f db.AnalyticsFilter,
	terms []db.TrendTermInput,
	granularity string,
) (db.TrendsTermsResponse, error) {
	if granularity == "" {
		granularity = "week"
	}
	acc := db.NewTrendAccumulator(f.From, f.To, granularity, terms)
	loc := analyticsLocation(f)
	sessionFilter := f
	sessionFilter.From = ""
	sessionFilter.To = ""
	sessionFilter.DayOfWeek = nil
	sessionFilter.Hour = nil
	sessionFilter.Model = ""
	pb := &paramBuilder{}
	preds := []string{buildAnalyticsWhereWithDate(
		sessionFilter, "", pb, false, "s.id", s.sessionDialect(),
	)}
	// The window bounds are inlined as literals. pgx caches prepared
	// statements per connection, and from the sixth run PostgreSQL may switch
	// to a generic plan that cannot see how narrow the window is.
	if sessionWindow := pgAnalyticsMessageWindowLiteral(f, pgDateCol); sessionWindow != "" {
		messageWindow := pgAnalyticsMessageWindowLiteral(f, "wm.timestamp")
		preds = append(preds, "s.id IN (SELECT sessions.id FROM sessions WHERE ("+sessionWindow+" OR EXISTS (SELECT 1 FROM messages wm WHERE wm.session_id = sessions.id AND wm.timestamp IS NOT NULL AND "+messageWindow+")))")
	}
	flt := f.MessageScopeFilter()
	modelFiltering := len(flt.Models) > 0
	content := "m.content"
	if inWindow := pgAnalyticsMessageWindowLiteral(f, "COALESCE(m.timestamp, s.started_at, s.created_at)"); inWindow != "" {
		rowWindow := inWindow
		if modelFiltering {
			content = "CASE WHEN " + inWindow + " THEN m.content ELSE '' END"
			rowWindow = "(" + inWindow + " OR m.role = 'assistant')"
		}
		preds = append(preds, rowWindow)
	}
	// A model filter credits a prompt to the next assistant reply, so the
	// scope reducer needs rows in order. Without one, rows count on their
	// own, and the sort spills to disk on large ranges.
	tail := ""
	if modelFiltering {
		tail = `
		ORDER BY m.session_id, m.ordinal`
	}
	query := `SELECT m.session_id, m.ordinal, m.role, m.is_system,
			COALESCE(m.model, ''), ` + content + `,
			COALESCE(TO_CHAR(m.timestamp AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(TO_CHAR(s.started_at AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(TO_CHAR(s.created_at AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		FROM sessions s
		JOIN messages m ON m.session_id = s.id
		WHERE ` + strings.Join(preds, " AND ") + `
			AND m.role IN ('user', 'assistant')
			AND m.is_system = FALSE
			AND ` + db.PostgresSystemPrefixSQL("m.content", "m.role") + tail

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.TrendsTermsResponse{}, fmt.Errorf("querying trends terms: %w", err)
	}
	defer rows.Close()

	type trendRow struct {
		sessionID string
		role      string
		isSystem  bool
		model     string
		content   string
		msgTS     string
		startedAt string
		createdAt string
	}
	processRow := func(row trendRow) {
		msgTime, ok := trendMessageLocalTime(row.msgTS, row.startedAt, row.createdAt, loc)
		if !ok {
			return
		}
		acc.Add(row.content, msgTime)
	}
	rowStartedAt := make(map[string]string)
	rowCreatedAt := make(map[string]string)
	emit := func(m db.ScopedMessage) {
		processRow(trendRow{
			sessionID: m.SessionID,
			role:      m.Role,
			isSystem:  m.IsSystem,
			content:   m.Content,
			msgTS:     m.Timestamp,
			startedAt: rowStartedAt[m.SessionID],
			createdAt: rowCreatedAt[m.SessionID],
		})
	}
	reducer := db.NewScopeReducer(flt, emit)

	for rows.Next() {
		var row trendRow
		var ordinal int
		if err := rows.Scan(
			&row.sessionID, &ordinal, &row.role, &row.isSystem, &row.model,
			&row.content, &row.msgTS, &row.startedAt, &row.createdAt,
		); err != nil {
			return db.TrendsTermsResponse{}, fmt.Errorf("scanning trends term row: %w", err)
		}
		if !modelFiltering {
			msgTime, ok := trendMessageLocalTime(row.msgTS, row.startedAt, row.createdAt, loc)
			if ok && flt.MatchesDayHour(msgTime, true) {
				processRow(row)
			}
			continue
		}
		rowStartedAt[row.sessionID] = row.startedAt
		rowCreatedAt[row.sessionID] = row.createdAt
		msgTime, has := trendMessageLocalTime(row.msgTS, row.startedAt, row.createdAt, loc)
		if err := reducer.Push(db.MessageInput{
			SessionID:    row.sessionID,
			Ordinal:      ordinal,
			Role:         row.role,
			Model:        row.model,
			IsSystem:     row.isSystem,
			Timestamp:    row.msgTS,
			LocalTime:    msgTime,
			HasLocalTime: has,
			Content:      row.content,
		}); err != nil {
			return db.TrendsTermsResponse{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return db.TrendsTermsResponse{}, fmt.Errorf("iterating trends term rows: %w", err)
	}

	return acc.Response(), nil
}

func trendMessageLocalTime(
	messageTS string,
	startedAt string,
	createdAt string,
	loc *time.Location,
) (time.Time, bool) {
	for _, ts := range []string{messageTS, startedAt, createdAt} {
		if t, ok := db.LocalTime(ts, loc); ok {
			return t, true
		}
	}
	return time.Time{}, false
}
