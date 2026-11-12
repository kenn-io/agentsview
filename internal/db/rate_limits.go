package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

// RateLimitTimeLayout is the fixed-width UTC layout of rate-limit times, so
// stored text sorts in time order.
const RateLimitTimeLayout = "2006-01-02T15:04:05.000Z"

// FormatRateLimitTime renders t in RateLimitTimeLayout.
func FormatRateLimitTime(t time.Time) string {
	return t.UTC().Format(RateLimitTimeLayout)
}

// RateLimitSnapshot is one subscription rate-limit report an agent recorded:
// how much of each limit window an account had used, and when each window
// resets, as of ObservedAt. Rows are stored in rate_limit_snapshots, one per
// change in the reported values within a session. Times are
// RateLimitTimeLayout strings.
type RateLimitSnapshot struct {
	ID int64 `json:"-"`
	// Agent is the agent that recorded the report, for example "codex".
	Agent string `json:"agent"`
	// AccountKey is an opaque key for the account the session recorded;
	// empty when it recorded none. It separates accounts and identifies
	// nothing else.
	AccountKey string `json:"account_key"`
	// LimitID names the limit as the agent reported it.
	LimitID string `json:"limit_id"`
	// PlanType is the plan the agent reported, empty when it reported none.
	PlanType string `json:"plan_type"`
	// SessionID is the session whose record carried the report.
	SessionID  string           `json:"session_id"`
	ObservedAt string           `json:"observed_at"`
	Primary    *RateLimitWindow `json:"primary,omitempty"`
	Secondary  *RateLimitWindow `json:"secondary,omitempty"`
}

// RateLimitWindow is one window of a rate-limit report.
type RateLimitWindow struct {
	// UsedPercent is the share of the window already used, 0 to 100.
	UsedPercent float64 `json:"used_percent"`
	// WindowMinutes is the window length the agent reported, absent when
	// it reported none.
	WindowMinutes *int64 `json:"window_minutes,omitempty"`
	// ResetsAt is when the window resets, empty when the agent reported
	// no reset time.
	ResetsAt string `json:"resets_at,omitempty"`
}

// RateLimitSnapshotsFromParsed converts a session's parsed rate-limit reports
// into rows for the given agent and stored session ID.
func RateLimitSnapshotsFromParsed(
	agent, sessionID string, parsed []parser.ParsedRateLimitSnapshot,
) []RateLimitSnapshot {
	if len(parsed) == 0 {
		return nil
	}
	window := func(w *parser.ParsedRateLimitWindow) *RateLimitWindow {
		if w == nil {
			return nil
		}
		out := &RateLimitWindow{UsedPercent: w.UsedPercent}
		if w.WindowMinutes != nil {
			minutes := *w.WindowMinutes
			out.WindowMinutes = &minutes
		}
		if w.ResetsAt != nil {
			out.ResetsAt = FormatRateLimitTime(*w.ResetsAt)
		}
		return out
	}
	out := make([]RateLimitSnapshot, 0, len(parsed))
	for _, p := range parsed {
		out = append(out, RateLimitSnapshot{
			Agent:      agent,
			AccountKey: p.AccountKey,
			LimitID:    p.LimitID,
			PlanType:   p.PlanType,
			SessionID:  sessionID,
			ObservedAt: FormatRateLimitTime(p.ObservedAt),
			Primary:    window(p.Primary),
			Secondary:  window(p.Secondary),
		})
	}
	return out
}

// RateLimitWindowFromColumns builds a window from its nullable stored
// columns, or nil when the report carried no such window.
func RateLimitWindowFromColumns(
	used sql.NullFloat64, minutes sql.NullInt64, resetsAt string,
) *RateLimitWindow {
	if !used.Valid {
		return nil
	}
	w := &RateLimitWindow{UsedPercent: used.Float64, ResetsAt: resetsAt}
	if minutes.Valid {
		v := minutes.Int64
		w.WindowMinutes = &v
	}
	return w
}

// RateLimitWindowColumns returns a window's stored column values: NULL for
// an absent window, minutes, or reset time.
func RateLimitWindowColumns(w *RateLimitWindow) (used, minutes, resetsAt any) {
	if w == nil {
		return nil, nil, nil
	}
	used = w.UsedPercent
	if w.WindowMinutes != nil {
		minutes = *w.WindowMinutes
	}
	if w.ResetsAt != "" {
		resetsAt = w.ResetsAt
	}
	return used, minutes, resetsAt
}

// LatestRateLimitSnapshotsSQL selects, for each agent, account, and limit,
// the most recent report: the one with the latest observed_at, ties broken
// by session_id. Every backend runs it over its rate_limit_snapshots table
// and scans the columns in this order.
const LatestRateLimitSnapshotsSQL = `
	SELECT agent, account_key, limit_id, plan_type, session_id, observed_at,
		primary_used_percent, primary_window_minutes, primary_resets_at,
		secondary_used_percent, secondary_window_minutes, secondary_resets_at
	FROM (
		SELECT agent, account_key, limit_id, plan_type, session_id, observed_at,
			primary_used_percent, primary_window_minutes, primary_resets_at,
			secondary_used_percent, secondary_window_minutes, secondary_resets_at,
			ROW_NUMBER() OVER (
				PARTITION BY agent, account_key, limit_id
				ORDER BY observed_at DESC, session_id DESC
			) AS rate_limit_rank
		FROM rate_limit_snapshots
	) latest
	WHERE rate_limit_rank = 1
	ORDER BY agent, observed_at DESC, account_key, limit_id`

// InsertRateLimitSnapshots appends rate-limit reports. A report already
// stored for the same session, limit, and time is ignored, so reparsing a
// source adds nothing.
func (db *DB) InsertRateLimitSnapshots(
	ctx context.Context, snaps []RateLimitSnapshot,
) error {
	if len(snaps) == 0 {
		return nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning rate limit tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range snaps {
		if s.Agent == "" || s.SessionID == "" || s.ObservedAt == "" {
			return fmt.Errorf("rate limit snapshot needs agent, session, and time")
		}
		pUsed, pMinutes, pResets := RateLimitWindowColumns(s.Primary)
		sUsed, sMinutes, sResets := RateLimitWindowColumns(s.Secondary)
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO rate_limit_snapshots (
				agent, account_key, limit_id, plan_type, session_id, observed_at,
				primary_used_percent, primary_window_minutes, primary_resets_at,
				secondary_used_percent, secondary_window_minutes, secondary_resets_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			SanitizeUTF8(s.Agent), SanitizeUTF8(s.AccountKey),
			SanitizeUTF8(s.LimitID), SanitizeUTF8(s.PlanType),
			s.SessionID, s.ObservedAt,
			pUsed, pMinutes, pResets, sUsed, sMinutes, sResets,
		); err != nil {
			return fmt.Errorf("inserting rate limit snapshot: %w", err)
		}
	}
	return tx.Commit()
}

func (db *DB) hasRateLimitTable(ctx context.Context) bool {
	var n int
	err := db.getReader().QueryRow(ctx,
		"SELECT 1 FROM sqlite_master WHERE type='table' AND name='rate_limit_snapshots'",
	).Scan(&n)
	return err == nil && n == 1
}

// GetLatestRateLimits returns the most recent rate-limit report for each
// agent, account, and limit. An archive without the table, opened read-only
// by this build, has none.
func (db *DB) GetLatestRateLimits(ctx context.Context) ([]RateLimitSnapshot, error) {
	if !db.hasRateLimitTable(ctx) {
		return []RateLimitSnapshot{}, nil
	}
	rows, err := db.getReader().QueryContext(ctx, LatestRateLimitSnapshotsSQL)
	if err != nil {
		return nil, fmt.Errorf("querying latest rate limits: %w", err)
	}
	return scanSQLiteRateLimitSnapshots(rows, false)
}

// MaxRateLimitSnapshotID returns the largest rate-limit row id, or 0 when
// there are none. Mirrors compare it with their high-water id.
func (db *DB) MaxRateLimitSnapshotID(ctx context.Context) (int64, error) {
	if !db.hasRateLimitTable(ctx) {
		return 0, nil
	}
	var id int64
	if err := db.getReader().QueryRow(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM rate_limit_snapshots`).Scan(&id); err != nil {
		return 0, fmt.Errorf("querying max rate limit snapshot id: %w", err)
	}
	return id, nil
}

// GetRateLimitSnapshots returns rate-limit rows with id greater than sinceID,
// in id order. The table is append-only, so sinceID works as a high-water
// mark: pass 0 for the whole history.
func (db *DB) GetRateLimitSnapshots(
	ctx context.Context, sinceID int64,
) ([]RateLimitSnapshot, error) {
	if !db.hasRateLimitTable(ctx) {
		return nil, nil
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id, agent, account_key, limit_id, plan_type, session_id, observed_at,
			primary_used_percent, primary_window_minutes, primary_resets_at,
			secondary_used_percent, secondary_window_minutes, secondary_resets_at
		FROM rate_limit_snapshots
		WHERE id > ?
		ORDER BY id`, sinceID)
	if err != nil {
		return nil, fmt.Errorf("querying rate limit snapshots: %w", err)
	}
	return scanSQLiteRateLimitSnapshots(rows, true)
}

func scanSQLiteRateLimitSnapshots(rows *sql.Rows, withID bool) ([]RateLimitSnapshot, error) {
	defer rows.Close()
	out := []RateLimitSnapshot{}
	for rows.Next() {
		var s RateLimitSnapshot
		var pUsed, sUsed sql.NullFloat64
		var pMinutes, sMinutes sql.NullInt64
		var pResets, sResets sql.NullString
		dest := []any{
			&s.Agent, &s.AccountKey, &s.LimitID, &s.PlanType, &s.SessionID, &s.ObservedAt,
			&pUsed, &pMinutes, &pResets, &sUsed, &sMinutes, &sResets,
		}
		if withID {
			dest = append([]any{&s.ID}, dest...)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scanning rate limit snapshot: %w", err)
		}
		s.Primary = RateLimitWindowFromColumns(pUsed, pMinutes, pResets.String)
		s.Secondary = RateLimitWindowFromColumns(sUsed, sMinutes, sResets.String)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rate limit snapshots: %w", err)
	}
	return out, nil
}
