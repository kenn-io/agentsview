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

// sqliteTrendSelection selects the sessions a Trends read walks.
func sqliteTrendSelection(f AnalyticsFilter) (string, []any) {
	sessionFilter := f
	sessionFilter.From, sessionFilter.To, sessionFilter.Model = "", "", ""
	sessionFilter.DayOfWeek, sessionFilter.Hour = nil, nil
	where, args := sessionFilter.buildWhereWithDate("", false, "s.id")
	from, to := f.messageWindowBoundsUTC()
	if sessionWindow, wargs := analyticsMessageWindowPred("COALESCE(NULLIF(started_at, ''), created_at)", from, to); sessionWindow != "" {
		messageWindow, messageArgs := analyticsMessageWindowPred("wm.timestamp", from, to)
		// Filter sessions before walking their messages.
		where += " AND s.id IN (SELECT sessions.id FROM sessions WHERE (" + sessionWindow + " OR EXISTS (SELECT 1 FROM messages wm WHERE wm.session_id = sessions.id AND wm.timestamp != '' AND " + messageWindow + ")))"
		args = append(args, wargs...)
		args = append(args, messageArgs...)
	}
	return where, args
}

func (db *DB) TrendSessionVersions(ctx context.Context, f AnalyticsFilter) (map[string]string, error) {
	where, args := sqliteTrendSelection(f)
	return ScanTrendVersions(db.getReader().QueryContext(ctx, `SELECT s.id, s.transcript_revision || '|' || s.created_at || '|' || COALESCE(s.started_at, '') FROM sessions s WHERE `+where, args...))
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
