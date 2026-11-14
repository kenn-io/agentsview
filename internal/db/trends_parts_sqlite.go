package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"time"
)

func (db *DB) trendsPartsDisk() {
	db.trendsDiskOnce.Do(func() {
		if db.readOnly || db.usageOnlyStorage() || db.path == "" {
			return
		}
		sum := sha256.Sum256([]byte(db.path))
		if db.trendsDisk.Open("sqlite-trends", hex.EncodeToString(sum[:8]), 128<<20, 256<<20) == nil {
			db.trendsDisk.Sweep(time.Now())
		}
	})
}

// Offset timestamps cannot be ordered against UTC bounds as text. Widen the
// candidate window by the largest RFC3339 offset Go accepts, then apply the
// exact local date and hour after parsing.
func sqliteTrendWindowPred(col, from, to string) (string, []any) {
	pred, args := analyticsMessageWindowPred(col, from, to)
	if pred == "" {
		return pred, args
	}
	offsetBound := func(value string, shift time.Duration) string {
		t, err := time.Parse("2006-01-02T15:04:05", value)
		if err != nil {
			return value
		}
		return t.Add(shift).Format("2006-01-02T15:04:05")
	}
	offsetRange, offsetArgs := analyticsMessageRangePred(col, offsetBound(from, -25*time.Hour), offsetBound(to, 25*time.Hour))
	return "(" + pred + " OR (substr(" + col + ", -6, 1) IN ('+', '-') AND " + offsetRange + "))", append(args, offsetArgs...)
}

// sqliteTrendSessionWindow selects sessions that start or have a message in the padded window.
func sqliteTrendSessionWindow(f AnalyticsFilter) (string, []any) {
	from, to := f.messageWindowBoundsUTC()
	sessionWindow, args := sqliteTrendWindowPred("COALESCE(NULLIF(started_at, ''), created_at)", from, to)
	if sessionWindow == "" {
		return "", nil
	}
	messageWindow, messageArgs := sqliteTrendWindowPred("wm.timestamp", from, to)
	return "(" + sessionWindow + " OR EXISTS (SELECT 1 FROM messages wm WHERE wm.session_id = sessions.id AND wm.timestamp != '' AND " + messageWindow + "))", append(args, messageArgs...)
}

func (db *DB) TrendSessionVersions(ctx context.Context, f AnalyticsFilter) (map[string]string, error) {
	where, args := f.TrendsSessionFilter().buildWhereWithDate("", false, "s.id")
	return ScanTrendVersions(db.getReader().QueryContext(ctx, `SELECT s.id, s.transcript_revision || '|' || COALESCE(s.trends_generation, '') || '|' || s.created_at || '|' || COALESCE(s.started_at, '') FROM sessions s WHERE `+where, args...))
}

func (db *DB) TrendWindowSessions(ctx context.Context, f AnalyticsFilter, ids []string) ([]string, error) {
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	query, args := `SELECT id FROM sessions WHERE id IN (SELECT value FROM json_each(?))`, []any{string(encoded)}
	if window, wargs := sqliteTrendSessionWindow(f); window != "" {
		query += " AND " + window
		args = append(args, wargs...)
	}
	return ScanTrendIDs(db.getReader().QueryContext(ctx, query, args...))
}

func (db *DB) VisitTrendMessages(ctx context.Context, f AnalyticsFilter, ids []string, visit func(string, string, time.Time)) error {
	encoded, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := db.getReader().QueryContext(ctx, `SELECT m.session_id, m.content, agentsview_trend_timestamp(COALESCE(m.timestamp, ''), COALESCE(s.started_at, ''), COALESCE(s.created_at, '')) FROM messages m JOIN sessions s ON s.id = m.session_id WHERE m.session_id IN (SELECT value FROM json_each(?)) AND m.role IN ('user', 'assistant') AND m.is_system = 0 AND `+SystemPrefixSQL("m.content", "m.role")+` ORDER BY m.session_id, m.ordinal`, string(encoded))
	if err != nil {
		return err
	}
	defer rows.Close()
	loc := f.location()
	for rows.Next() {
		var id, content, timestamp string
		if err := rows.Scan(&id, &content, &timestamp); err != nil {
			return err
		}
		if local, ok := LocalTime(timestamp, loc); ok {
			visit(id, content, local)
		}
	}
	return rows.Err()
}
