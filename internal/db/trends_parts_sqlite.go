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
			return ""
		}
		t = t.Add(shift)
		if t.Year() < 1 || t.Year() > 9999 {
			return ""
		}
		return t.Format("2006-01-02T15:04:05")
	}
	offsetRange, offsetArgs := analyticsMessageRangePred(col, offsetBound(from, -25*time.Hour), offsetBound(to, 25*time.Hour))
	if offsetRange == "" {
		return "(" + pred + " OR substr(" + col + ", -6, 1) IN ('+', '-'))", args
	}
	return "(" + pred + " OR (substr(" + col + ", -6, 1) IN ('+', '-') AND " + offsetRange + "))", append(args, offsetArgs...)
}

// sqliteTrendSelection selects the sessions a Trends read walks.
func sqliteTrendSelection(f AnalyticsFilter, window bool) (string, []any) {
	sessionFilter := f
	sessionFilter.From, sessionFilter.To, sessionFilter.Model = "", "", ""
	sessionFilter.DayOfWeek, sessionFilter.Hour = nil, nil
	where, args := sessionFilter.buildWhereWithDate("", false, "s.id")
	from, to := f.messageWindowBoundsUTC()
	if !window {
		return where, args
	}
	if sessionWindow, wargs := sqliteTrendWindowPred("COALESCE(NULLIF(started_at, ''), created_at)", from, to); sessionWindow != "" {
		messageWindow, messageArgs := sqliteTrendWindowPred("wm.timestamp", from, to)
		// Filter sessions before walking their messages.
		where += " AND s.id IN (SELECT sessions.id FROM sessions WHERE (" + sessionWindow + " OR EXISTS (SELECT 1 FROM messages wm WHERE wm.session_id = sessions.id AND wm.timestamp != '' AND " + messageWindow + ")))"
		args = append(args, wargs...)
		args = append(args, messageArgs...)
	}
	return where, args
}

func (db *DB) TrendSessionVersions(ctx context.Context, f AnalyticsFilter) (map[string]string, error) {
	where, args := sqliteTrendSelection(f, false)
	return ScanTrendVersions(db.getReader().QueryContext(ctx, `SELECT s.id, s.transcript_revision || '|' || s.created_at || '|' || COALESCE(s.started_at, '') FROM sessions s WHERE `+where, args...))
}

func (db *DB) TrendWindowSessions(ctx context.Context, f AnalyticsFilter, ids []string) ([]string, error) {
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	from, to := f.messageWindowBoundsUTC()
	session, args := sqliteTrendWindowPred("COALESCE(NULLIF(s.started_at, ''), s.created_at)", from, to)
	where := ""
	if session != "" {
		message, more := sqliteTrendWindowPred("wm.timestamp", from, to)
		where = " AND (" + session + " OR EXISTS (SELECT 1 FROM messages wm WHERE wm.session_id = s.id AND wm.timestamp != '' AND " + message + "))"
		args = append(args, more...)
	}
	args = append([]any{string(encoded)}, args...)
	rows, err := db.getReader().QueryContext(ctx, `SELECT s.id FROM sessions s WHERE s.id IN (SELECT value FROM json_each(?))`+where+` ORDER BY s.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var selected []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		selected = append(selected, id)
	}
	return selected, rows.Err()
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
