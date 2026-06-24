package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CursorUsageEvent stores authoritative Cursor admin usage data.
// The table is append-only, deduped by a stable event fingerprint.
type CursorUsageEvent struct {
	ID               int64
	OccurredAt       string
	Model            string
	Kind             string
	InputTokens      int
	OutputTokens     int
	CacheWriteTokens int
	CacheReadTokens  int
	ChargedCents     float64
	CursorTokenFee   float64
	UserID           string
	UserEmail        string
	IsHeadless       bool
	DedupKey         string
}

func (db *DB) ensureCursorUsageEventsSchemaLocked(w *sql.DB) error {
	if _, err := w.Exec(`
		CREATE TABLE IF NOT EXISTS cursor_usage_events (
			id INTEGER PRIMARY KEY,
			occurred_at TEXT NOT NULL,
			model TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			charged_cents REAL NOT NULL DEFAULT 0,
			cursor_token_fee REAL NOT NULL DEFAULT 0,
			user_id TEXT NOT NULL DEFAULT '',
			user_email TEXT NOT NULL DEFAULT '',
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

// InsertCursorUsageEvents appends new Cursor usage rows and ignores
// duplicates with the same stable fingerprint.
func (db *DB) InsertCursorUsageEvents(
	events []CursorUsageEvent,
) error {
	if len(events) == 0 {
		return nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning cursor usage tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, ev := range events {
		if ev.Model == "" {
			return fmt.Errorf("cursor usage event model is required")
		}
		if ev.OccurredAt == "" {
			return fmt.Errorf("cursor usage event timestamp is required")
		}
		if ev.DedupKey == "" {
			ev.DedupKey = cursorUsageEventDedupKey(ev)
		}
		if ev.DedupKey == "" {
			return fmt.Errorf("cursor usage event dedup key is required")
		}

		isHeadless := 0
		if ev.IsHeadless {
			isHeadless = 1
		}

		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO cursor_usage_events (
				occurred_at, model, kind,
				input_tokens, output_tokens,
				cache_write_tokens, cache_read_tokens,
				charged_cents, cursor_token_fee,
				user_id, user_email, is_headless, dedup_key
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.OccurredAt, SanitizeUTF8(ev.Model), SanitizeUTF8(ev.Kind),
			ev.InputTokens, ev.OutputTokens,
			ev.CacheWriteTokens, ev.CacheReadTokens,
			ev.ChargedCents, ev.CursorTokenFee,
			SanitizeUTF8(ev.UserID), SanitizeUTF8(ev.UserEmail),
			isHeadless, ev.DedupKey,
		); err != nil {
			return fmt.Errorf("inserting cursor usage event: %w", err)
		}
	}

	return tx.Commit()
}

func cursorUsageEventDedupKey(ev CursorUsageEvent) string {
	var b strings.Builder
	b.Grow(256)
	fmt.Fprintf(&b, "%s|%s|%s|%d|%d|%d|%d|%s|%s|%t|%s|%s",
		ev.OccurredAt,
		SanitizeUTF8(ev.Model),
		SanitizeUTF8(ev.Kind),
		ev.InputTokens,
		ev.OutputTokens,
		ev.CacheWriteTokens,
		ev.CacheReadTokens,
		strconv.FormatFloat(ev.ChargedCents, 'f', -1, 64),
		strconv.FormatFloat(ev.CursorTokenFee, 'f', -1, 64),
		ev.IsHeadless,
		SanitizeUTF8(ev.UserID),
		SanitizeUTF8(ev.UserEmail),
	)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func cursorUsageOccurredAtFromMillis(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty timestamp")
	}
	if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(0, ms*int64(time.Millisecond)).
			UTC().Format(time.RFC3339Nano), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	return "", fmt.Errorf("parsing timestamp %q", raw)
}
