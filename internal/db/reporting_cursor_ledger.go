package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
)

type reportingCursorEventRow struct {
	at    time.Time
	event export.ReportingCursorEvent
	usage activity.UsageRow
}

type reportingCursorPollRow struct {
	at   time.Time
	poll export.ReportingCursorPoll
}

func (db *DB) loadReportingCursorLedger(ctx context.Context, tx *sql.Tx, start, end time.Time, source *reportingExportSource) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='cursor_usage_events')`).Scan(&exists); err != nil {
		return err
	}
	if !exists || !start.Before(end) {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT agentsview_timestamp_unix_micro(occurred_at), COALESCE(NULLIF(model, ''), '<unknown>'), input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, charged_microdollars, dedup_key, account_user_id, account_polled_at FROM cursor_usage_events WHERE agent='cursor' AND occurred_at >= ? AND occurred_at <= ? AND agentsview_timestamp_unix_micro(occurred_at) >= ? AND agentsview_timestamp_unix_micro(occurred_at) < ?`, PaddedUTCBound(start.Format(time.RFC3339), -14), PaddedUTCBound(end.Format(time.RFC3339), 14), start.UnixMicro(), end.UnixMicro())
	if err != nil {
		return fmt.Errorf("querying Cursor reporting events: %w", err)
	}
	defer rows.Close()
	type pollKey struct {
		at      time.Time
		account string
	}
	latest := map[pollKey]string{}
	for rows.Next() {
		var row reportingCursorEventRow
		var micros int64
		e := &row.event
		if err := rows.Scan(&micros, &e.Model, &e.InputTokens, &e.OutputTokens, &e.CacheWriteTokens, &e.CacheReadTokens, &e.Cost.Microdollars, &e.Key, &e.Account, &e.PolledAt); err != nil {
			return err
		}
		row.at = time.UnixMicro(micros).UTC()

		e.InputTokens, e.OutputTokens = max(e.InputTokens, 0), max(e.OutputTokens, 0)
		e.CacheWriteTokens, e.CacheReadTokens = max(e.CacheWriteTokens, 0), max(e.CacheReadTokens, 0)
		row.usage = activity.UsageRow{Timestamp: row.at.Format(time.RFC3339Nano), Model: e.Model, Agent: "cursor", UsageDedupKey: e.Key}
		row.usage.Cost = e.Cost
		setStandaloneUsageFields(&row.usage, int(e.InputTokens), int(e.OutputTokens), int(e.CacheWriteTokens), int(e.CacheReadTokens))
		source.cursorEvents = append(source.cursorEvents, row)
		if e.Account != "" && e.PolledAt != "" {
			key := pollKey{row.at.Truncate(time.Hour), e.Account}
			if e.PolledAt > latest[key] {
				latest[key] = e.PolledAt
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	polls, err := tx.QueryContext(ctx, `SELECT account_user_id, hour_start, polled_at FROM cursor_account_polls WHERE hour_start >= ? AND hour_start < ?`, start.Unix(), end.Unix())
	if err != nil {
		return fmt.Errorf("querying Cursor reporting polls: %w", err)
	}
	defer polls.Close()
	for polls.Next() {
		var row reportingCursorPollRow
		var seconds int64
		if err := polls.Scan(&row.poll.Account, &seconds, &row.poll.PolledAt); err != nil {
			return err
		}
		row.at = time.Unix(seconds, 0).UTC()
		key := pollKey{row.at, row.poll.Account}
		if row.poll.PolledAt > latest[key] {
			latest[key] = row.poll.PolledAt
		}
	}
	if err := polls.Err(); err != nil {
		return err
	}
	for key, stamp := range latest {
		source.cursorPolls = append(source.cursorPolls, reportingCursorPollRow{at: key.at, poll: export.ReportingCursorPoll{Account: key.account, PolledAt: stamp}})
	}
	return nil
}
