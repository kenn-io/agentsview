package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ReportingRange discovers dated evidence in closed UTC reporting hours.
// Its schema is independent of the hour, day, and digest schemas.
type ReportingRange struct {
	SchemaVersion int     `json:"schema_version"`
	EarliestDate  *string `json:"earliest_date"`
	ClosedThrough string  `json:"closed_through"`
}

// ExportReportingRange returns a conservative starting date and an exclusive
// closed-hour cutoff. It reads timestamp metadata in one SQLite statement;
// discovery does not aggregate bodies, deduplicate usage, or promise completeness.
func (db *DB) ExportReportingRange(ctx context.Context, now time.Time) (ReportingRange, error) {
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.UTC().Truncate(time.Hour)
	result := ReportingRange{SchemaVersion: 1, ClosedThrough: cutoff.Format(time.RFC3339)}
	var earliest sql.NullInt64
	// Keep the activity eligibility used by reportingHoursFromSnapshot. Usage
	// eligibility is independent: a session with no activity may still have cost.
	// ponytail: scan timestamp metadata; index normalized bounds if discovery
	// becomes a measured bottleneck on large archives.
	activityWhere, _ := (AnalyticsFilter{IncludeSubagents: true, IncludeForks: true}).buildWhereWithDate("", false, "s.id")
	reader := db.getReader()
	var hasCursorUsage bool
	err := reader.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'cursor_usage_events'
	)`).Scan(&hasCursorUsage)
	if err != nil {
		return ReportingRange{}, fmt.Errorf("checking standalone reporting usage table: %w", err)
	}
	standaloneUsage := ""
	if hasCursorUsage {
		standaloneUsage = `UNION ALL
			SELECT agentsview_timestamp_unix_micro(occurred_at)
			FROM cursor_usage_events WHERE model != ''`
	}
	err = reader.QueryRowContext(ctx, `
		WITH timestamps(ts) AS (
			SELECT COALESCE(agentsview_timestamp_unix_micro(COALESCE(s.started_at, '')),
				agentsview_timestamp_unix_micro(s.created_at))
			FROM sessions s WHERE `+activityWhere+`
			UNION ALL
			SELECT agentsview_timestamp_unix_micro(COALESCE(
				NULLIF(m.timestamp, ''), NULLIF(s.started_at, ''), s.created_at))
			FROM messages m JOIN sessions s ON s.id = m.session_id
			WHERE (`+activityWhere+`) OR (`+usageMessageEligibility+`)
			UNION ALL
			SELECT agentsview_timestamp_unix_micro(COALESCE(
				NULLIF(ue.occurred_at, ''), NULLIF(s.started_at, ''), s.created_at))
			FROM usage_events ue JOIN sessions s ON s.id = ue.session_id
			WHERE `+usageEventEligibility+`
			UNION ALL
			SELECT agentsview_timestamp_unix_micro(COALESCE(
				NULLIF(tre.timestamp, ''), NULLIF(s.started_at, ''), s.created_at))
			FROM tool_result_events tre JOIN sessions s ON s.id = tre.session_id
			WHERE `+activityWhere+` AND tre.source = 'tool_execution'
				AND tre.status IN ('completed', 'errored')
			`+standaloneUsage+`
		)
		SELECT MIN(ts) FROM timestamps`).Scan(&earliest)
	if err != nil {
		return ReportingRange{}, fmt.Errorf("query reporting range: %w", err)
	}
	if earliest.Valid && earliest.Int64 < cutoff.UnixMicro() {
		result.EarliestDate = new(time.UnixMicro(earliest.Int64).UTC().Format(time.DateOnly))
	}
	return result, nil
}
