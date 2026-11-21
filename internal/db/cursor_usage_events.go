package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/money"
)

// CursorUsageEvent stores authoritative account billing data. Agent defaults
// to cursor for Cursor admin API imports; xAI CSV imports identify each product.
// The table is append-only, deduped by a stable event fingerprint.
type CursorUsageEvent struct {
	ID               int64
	Agent            string
	OccurredAt       string
	Model            string
	Kind             string
	InputTokens      int
	OutputTokens     int
	CacheWriteTokens int
	CacheReadTokens  int
	Charged          money.Money
	CursorTokenFee   money.Money
	UserID           string
	UserEmail        string
	IsHeadless       bool
	DedupKey         string
}

const cursorAccountPollsSchemaSQL = `
CREATE TABLE IF NOT EXISTS cursor_account_polls (
    account_user_id TEXT NOT NULL,
    hour_start INTEGER NOT NULL,
    polled_at TEXT NOT NULL,
    PRIMARY KEY (account_user_id, hour_start)
);`

func (db *DB) ensureCursorUsageEventsSchemaLocked(ctx context.Context, w *writerHandle) error {
	if _, err := w.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS cursor_usage_events (
			id INTEGER PRIMARY KEY,
			agent TEXT NOT NULL DEFAULT 'cursor',
			occurred_at TEXT NOT NULL,
			model TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			charged_microdollars INTEGER NOT NULL DEFAULT 0,
			cursor_token_fee_microdollars INTEGER NOT NULL DEFAULT 0,
			user_id TEXT NOT NULL DEFAULT '',
			user_email TEXT NOT NULL DEFAULT '',
			account_user_id TEXT NOT NULL DEFAULT '',
			account_polled_at TEXT NOT NULL DEFAULT '',
			is_headless INTEGER NOT NULL DEFAULT 0,
			dedup_key TEXT NOT NULL DEFAULT ''
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_cursor_usage_events_dedup
			ON cursor_usage_events(dedup_key)
			WHERE dedup_key != '';
		CREATE INDEX IF NOT EXISTS idx_cursor_usage_events_occurred
			ON cursor_usage_events(occurred_at);
		CREATE INDEX IF NOT EXISTS idx_cursor_usage_events_model
			ON cursor_usage_events(model);
	`); err != nil {
		return fmt.Errorf("creating cursor_usage_events: %w", err)
	}
	return nil
}

var ErrCursorAccountRebuildPending = errors.New("Cursor account reporting requires the pending archive rebuild")

type CursorAccountHour struct {
	UserID string
	Start  time.Time
}

// CursorAccountHours returns previously attributed hours within a poll range.
func (db *DB) CursorAccountHours(ctx context.Context, start, end time.Time) ([]CursorAccountHour, error) {
	rows, err := db.getReader().Query(ctx, `SELECT DISTINCT account_user_id, `+reportingCursorHour+` FROM cursor_usage_events WHERE agent='cursor' AND account_user_id != '' AND occurred_at >= ? AND occurred_at < ? AND agentsview_timestamp_unix_micro(occurred_at) >= ? AND agentsview_timestamp_unix_micro(occurred_at) < ?`, start.Add(-time.Second).UTC().Format(time.RFC3339Nano), end.Add(time.Second).UTC().Format(time.RFC3339Nano), start.UnixMicro(), end.UnixMicro())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hours []CursorAccountHour
	for rows.Next() {
		var id string
		var seconds int64
		if err := rows.Scan(&id, &seconds); err != nil {
			return nil, err
		}
		hour := CursorAccountHour{UserID: id, Start: time.Unix(seconds, 0).UTC()}
		if !hour.Start.Before(start) && !hour.Start.Add(time.Hour).After(end) {
			hours = append(hours, hour)
		}
	}
	return hours, rows.Err()
}

// RecordCursorAccountPoll commits matched events and completed prior hours together.
func (db *DB) RecordCursorAccountPoll(ctx context.Context, assignments map[string][]string, polledAt time.Time, hours []CursorAccountHour) error {
	stamp := polledAt.UTC().Format("2006-01-02T15:04:05.000000Z")
	return db.Update(ctx, func(tx *sql.Tx) error {
		for id, keys := range assignments {
			encoded, err := json.Marshal(keys)
			if err != nil {
				return err
			}
			if id == "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO cursor_account_polls(account_user_id,hour_start,polled_at) SELECT account_user_id, `+reportingCursorHour+`, ? FROM cursor_usage_events WHERE account_user_id != '' AND dedup_key IN (SELECT value FROM json_each(?)) AND account_polled_at < ? ON CONFLICT(account_user_id,hour_start) DO UPDATE SET polled_at = max(polled_at,excluded.polled_at)`, stamp, string(encoded), stamp); err != nil {
					return fmt.Errorf("recording previous Cursor account poll: %w", err)
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE cursor_usage_events SET account_user_id = ?, account_polled_at = ? WHERE dedup_key IN (SELECT value FROM json_each(?)) AND account_polled_at < ?`, id, stamp, string(encoded), stamp); err != nil {
				return fmt.Errorf("assigning Cursor usage account: %w", err)
			}
		}
		for _, hour := range hours {
			if _, err := tx.ExecContext(ctx, `INSERT INTO cursor_account_polls(account_user_id,hour_start,polled_at) VALUES (?,?,?) ON CONFLICT(account_user_id,hour_start) DO UPDATE SET polled_at = max(polled_at,excluded.polled_at)`, hour.UserID, hour.Start.Unix(), stamp); err != nil {
				return fmt.Errorf("recording Cursor account poll: %w", err)
			}
		}
		return nil
	})
}

// RequireCursorAccountStamping checks that older binaries will refuse this archive.
func (db *DB) RequireCursorAccountStamping(ctx context.Context) error {
	version, _, err := readUserVersion(ctx, db.getReader())
	if err != nil {
		return err
	}
	if version < dataVersion {
		return ErrCursorAccountRebuildPending
	}
	return nil
}

// InsertCursorUsageEvents appends account usage rows and ignores duplicates
// with the same billing identity or stable fingerprint.
func (db *DB) InsertCursorUsageEvents(ctx context.Context,
	events []CursorUsageEvent,
) error {
	if len(events) == 0 {
		return nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning cursor usage tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, ev := range events {
		if ev.Agent == "" {
			ev.Agent = "cursor"
		}
		if ev.OccurredAt == "" {
			return errors.New("cursor usage event timestamp is required")
		}
		if ev.DedupKey == "" {
			ev.DedupKey = CursorUsageEventDedupKey(ev)
		}
		if ev.DedupKey == "" {
			return errors.New("cursor usage event dedup key is required")
		}

		isHeadless := 0
		if ev.IsHeadless {
			isHeadless = 1
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO cursor_usage_events (
				agent, occurred_at, model, kind,
				input_tokens, output_tokens,
				cache_write_tokens, cache_read_tokens,
				charged_microdollars, cursor_token_fee_microdollars,
				user_id, user_email, is_headless, dedup_key
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			SanitizeUTF8(ev.Agent), ev.OccurredAt, SanitizeUTF8(ev.Model), SanitizeUTF8(ev.Kind),
			ev.InputTokens, ev.OutputTokens,
			ev.CacheWriteTokens, ev.CacheReadTokens,
			ev.Charged.Microdollars, ev.CursorTokenFee.Microdollars,
			SanitizeUTF8(ev.UserID), SanitizeUTF8(ev.UserEmail),
			isHeadless, ev.DedupKey,
		); err != nil {
			return fmt.Errorf("inserting cursor usage event: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	db.notifyCursorUsage()
	return nil
}

// CursorUsageEventDedupKey returns the stable cross-backend identity for a
// Cursor usage event.
func CursorUsageEventDedupKey(ev CursorUsageEvent) string {
	var b strings.Builder
	b.Grow(256)
	if ev.Agent != "" && ev.Agent != "cursor" {
		fmt.Fprintf(&b, "%s|", ev.Agent)
	}
	fmt.Fprintf(&b, "%s|%s|%s|%d|%d|%d|%d|%s|%s|%t|%s|%s",
		cursorUsageEventFingerprintTimestamp(ev.OccurredAt),
		SanitizeUTF8(ev.Model),
		SanitizeUTF8(ev.Kind),
		ev.InputTokens,
		ev.OutputTokens,
		ev.CacheWriteTokens,
		ev.CacheReadTokens,
		formatMicrodollarsAsLegacyCents(ev.Charged.Microdollars),
		formatMicrodollarsAsLegacyCents(ev.CursorTokenFee.Microdollars),
		ev.IsHeadless,
		SanitizeUTF8(ev.UserID),
		SanitizeUTF8(ev.UserEmail),
	)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func cursorUsageEventFingerprintTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

// formatMicrodollarsAsLegacyCents preserves the canonical decimal text used by
// the pre-microdollar Cursor fingerprint. Migrated rows are rekeyed after
// quantization, so a newly fetched copy hashes the same representable value.
func formatMicrodollarsAsLegacyCents(microdollars int64) string {
	negative := microdollars < 0
	magnitude := uint64(microdollars)
	if negative {
		magnitude = uint64(-(microdollars + 1)) + 1
	}

	whole := magnitude / 10_000
	fraction := magnitude % 10_000
	formatted := strconv.FormatUint(whole, 10)
	if fraction != 0 {
		fractional := strconv.FormatUint(fraction+10_000, 10)[1:]
		formatted += "." + strings.TrimRight(fractional, "0")
	}
	if negative {
		return "-" + formatted
	}
	return formatted
}

// MaxCursorUsageEventID returns the largest account usage row id, or 0
// when there are no rows. Mirrors compare it with their high-water id
// before asking their target for it.
func (db *DB) MaxCursorUsageEventID(ctx context.Context) (int64, error) {
	if !db.hasCursorUsageTable(ctx) {
		return 0, nil
	}
	var id int64
	if err := db.getReader().QueryRow(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM cursor_usage_events`).Scan(&id); err != nil {
		return 0, fmt.Errorf("querying max cursor usage event id: %w", err)
	}
	return id, nil
}

// GetCursorUsageEvents returns account usage rows with id greater than
// sinceID, in (occurred_at, id) order. Account stamps can update existing rows;
// their IDs stay unchanged. Pass 0 for the full history, or the largest
// previously consumed ID to load only appended rows.
func (db *DB) GetCursorUsageEvents(
	ctx context.Context, sinceID int64,
) ([]CursorUsageEvent, error) {
	if !db.hasCursorUsageTable(ctx) {
		return nil, nil
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id, agent, occurred_at, model, kind,
			input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens,
			charged_microdollars, cursor_token_fee_microdollars,
			user_id, user_email, is_headless, dedup_key
		FROM cursor_usage_events
		WHERE id > ?
		ORDER BY occurred_at, id`, sinceID)
	if err != nil {
		return nil, fmt.Errorf("querying cursor usage events: %w", err)
	}
	defer rows.Close()

	var out []CursorUsageEvent
	for rows.Next() {
		var ev CursorUsageEvent
		var isHeadless int
		if err := rows.Scan(
			&ev.ID, &ev.Agent, &ev.OccurredAt, &ev.Model, &ev.Kind,
			&ev.InputTokens, &ev.OutputTokens,
			&ev.CacheWriteTokens, &ev.CacheReadTokens,
			&ev.Charged, &ev.CursorTokenFee,
			&ev.UserID, &ev.UserEmail, &isHeadless, &ev.DedupKey,
		); err != nil {
			return nil, fmt.Errorf("scanning cursor usage event: %w", err)
		}
		ev.IsHeadless = isHeadless != 0
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating cursor usage events: %w", err)
	}
	return out, nil
}

func (db *DB) CursorUsageEventFingerprint(ctx context.Context) (string, error) {
	if !db.hasCursorUsageTable(ctx) {
		return "", nil
	}
	rows, err := db.getReader().Query(ctx, `
		SELECT agent, occurred_at, model, kind,
			input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens,
			charged_microdollars, cursor_token_fee_microdollars,
			user_id, user_email, is_headless, dedup_key
		FROM cursor_usage_events
		ORDER BY occurred_at, id`)
	if err != nil {
		return "", fmt.Errorf("querying cursor usage fingerprint: %w", err)
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ev CursorUsageEvent
		var isHeadless int
		if err := rows.Scan(
			&ev.Agent, &ev.OccurredAt, &ev.Model, &ev.Kind,
			&ev.InputTokens, &ev.OutputTokens,
			&ev.CacheWriteTokens, &ev.CacheReadTokens,
			&ev.Charged, &ev.CursorTokenFee,
			&ev.UserID, &ev.UserEmail, &isHeadless, &ev.DedupKey,
		); err != nil {
			return "", fmt.Errorf("scanning cursor usage fingerprint: %w", err)
		}
		ev.Model = SanitizeUTF8(ev.Model)
		ev.Kind = SanitizeUTF8(ev.Kind)
		ev.UserID = SanitizeUTF8(ev.UserID)
		ev.UserEmail = SanitizeUTF8(ev.UserEmail)
		ev.DedupKey = SanitizeUTF8(ev.DedupKey)
		if ev.Agent != "cursor" {
			fmt.Fprintf(&b, "%d:%s|", len(ev.Agent), ev.Agent)
		}
		fmt.Fprintf(&b, "%d:%s|%d:%s|%d:%s|%d|%d|%d|%d|%d|%d|%d:%s|%d:%s|%t|%d:%s;",
			len(ev.OccurredAt), ev.OccurredAt,
			len(ev.Model), ev.Model,
			len(ev.Kind), ev.Kind,
			ev.InputTokens,
			ev.OutputTokens,
			ev.CacheWriteTokens,
			ev.CacheReadTokens,
			ev.Charged.Microdollars,
			ev.CursorTokenFee.Microdollars,
			len(ev.UserID), ev.UserID,
			len(ev.UserEmail), ev.UserEmail,
			isHeadless != 0,
			len(ev.DedupKey), ev.DedupKey,
		)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterating cursor usage fingerprint: %w", err)
	}
	return b.String(), nil
}
