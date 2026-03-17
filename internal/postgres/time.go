package postgres

import (
	"fmt"
	"time"
)

// Common timestamp formats found in SQLite data.
var sqliteFormats = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.000Z",
	"2006-01-02T15:04:05Z",
	"2006-01-02 15:04:05",
}

// ParseSQLiteTimestamp parses an ISO-8601 text timestamp from
// SQLite into a time.Time. Returns zero time and false for
// empty strings or unparseable values.
func ParseSQLiteTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, f := range sqliteFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// FormatISO8601 formats a time.Time to ISO-8601 UTC string
// for JSON API responses.
func FormatISO8601(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// syncTimestampLayout uses microsecond precision to match
// PostgreSQL's timestamp resolution.
const syncTimestampLayout = "2006-01-02T15:04:05.000000Z"

// LocalSyncTimestampLayout uses millisecond precision to match
// SQLite's datetime resolution.
const LocalSyncTimestampLayout = "2006-01-02T15:04:05.000Z"

// FormatSyncTimestamp formats a time as a microsecond-precision
// UTC ISO-8601 string for PG sync watermarks.
func FormatSyncTimestamp(t time.Time) string {
	return t.UTC().Format(syncTimestampLayout)
}

// NormalizeSyncTimestamp parses a RFC3339Nano timestamp and
// re-formats it to microsecond precision for PG sync.
func NormalizeSyncTimestamp(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", err
	}
	return FormatSyncTimestamp(ts), nil
}

// NormalizeLocalSyncTimestamp parses a RFC3339Nano timestamp and
// re-formats it to millisecond precision for SQLite sync state.
func NormalizeLocalSyncTimestamp(
	value string,
) (string, error) {
	if value == "" {
		return "", nil
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", err
	}
	return ts.UTC().Format(LocalSyncTimestampLayout), nil
}

// PreviousLocalSyncTimestamp returns the timestamp 1ms before
// the given value, formatted at millisecond precision. This
// creates a non-overlapping boundary for incremental sync
// queries against SQLite.
func PreviousLocalSyncTimestamp(
	value string,
) (string, error) {
	if value == "" {
		return "", nil
	}
	ts, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", err
	}
	prev := ts.Add(-time.Millisecond)
	return prev.UTC().Format(LocalSyncTimestampLayout), nil
}

// SyncStateStore is the interface needed for normalizing local
// sync timestamps stored in SQLite.
type SyncStateStore interface {
	GetSyncState(key string) (string, error)
	SetSyncState(key, value string) error
}

// NormalizeLocalSyncStateTimestamps normalizes the last_push_at
// watermark in the local SQLite sync state to millisecond
// precision.
func NormalizeLocalSyncStateTimestamps(
	local SyncStateStore,
) error {
	value, err := local.GetSyncState("last_push_at")
	if err != nil {
		return fmt.Errorf("reading last_push_at: %w", err)
	}
	if value == "" {
		return nil
	}
	normalized, err := NormalizeLocalSyncTimestamp(value)
	if err != nil {
		return fmt.Errorf(
			"normalizing last_push_at: %w", err,
		)
	}
	if normalized == value {
		return nil
	}
	if err := local.SetSyncState(
		"last_push_at", normalized,
	); err != nil {
		return fmt.Errorf("writing last_push_at: %w", err)
	}
	return nil
}
