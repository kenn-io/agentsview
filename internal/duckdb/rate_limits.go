package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// syncRateLimitSnapshots appends the rate-limit reports the mirror has not
// consumed yet, tracked by a high-water id in mirror metadata
// (rateLimitMaxIDMetadataKey), as syncCursorUsageEvents does for account
// usage rows. A fresh rebuild file has no metadata, so its first sync loads
// the whole history; an overlap after a crash is deduped by the identity
// index.
func (s *Sync) syncRateLimitSnapshots(ctx context.Context) error {
	// Reports describe accounts, not projects, so a project-filtered push
	// cannot say which of them belong to it.
	if s.isFiltered() {
		return nil
	}
	stored, err := readMetadataKey(ctx, s.duck, rateLimitMaxIDMetadataKey)
	if err != nil {
		return err
	}
	var sinceID int64
	if stored != "" {
		if sinceID, err = strconv.ParseInt(stored, 10, 64); err != nil {
			return fmt.Errorf("parsing rate limit high-water id %q: %w", stored, err)
		}
	}
	snaps, err := s.local.GetRateLimitSnapshots(ctx, sinceID)
	if err != nil {
		return fmt.Errorf("loading local rate limit snapshots: %w", err)
	}
	if len(snaps) == 0 {
		return nil
	}
	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb rate limit sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.bulkInsertRateLimitSnapshots(ctx, tx, snaps); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb rate limit sync: %w", err)
	}
	maxID := sinceID
	for _, snap := range snaps {
		maxID = max(maxID, snap.ID)
	}
	return recordMetadataKey(ctx, s.duck, rateLimitMaxIDMetadataKey, strconv.FormatInt(maxID, 10))
}

// duckRateLimitTime converts a stored rate-limit time to a TIMESTAMP value,
// or NULL when it is empty.
func duckRateLimitTime(value string) (any, error) {
	if value == "" {
		return nil, nil
	}
	t, ok := parseTimestamp(value)
	if !ok {
		return nil, fmt.Errorf("parsing rate limit time %q", value)
	}
	return t.UTC(), nil
}

func (s *Sync) bulkInsertRateLimitSnapshots(
	ctx context.Context, tx *sql.Tx, snaps []db.RateLimitSnapshot,
) error {
	const batchSize = 100
	for i := 0; i < len(snaps); i += batchSize {
		batch := snaps[i:min(i+batchSize, len(snaps))]
		var b strings.Builder
		b.WriteString(`INSERT INTO rate_limit_snapshots (
			id, agent, account_key, limit_id, plan_type, session_id, observed_at,
			primary_used_percent, primary_window_minutes, primary_resets_at,
			secondary_used_percent, secondary_window_minutes, secondary_resets_at
		) VALUES `)
		args := make([]any, 0, len(batch)*13)
		for j, snap := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			observed, err := duckRateLimitTime(snap.ObservedAt)
			if err != nil {
				return err
			}
			if observed == nil {
				return fmt.Errorf("rate limit snapshot %d has no observed time", snap.ID)
			}
			args = append(args,
				snap.ID, db.SanitizeUTF8(snap.Agent), db.SanitizeUTF8(snap.AccountKey),
				db.SanitizeUTF8(snap.LimitID), db.SanitizeUTF8(snap.PlanType),
				snap.SessionID, observed,
			)
			for _, w := range []*db.RateLimitWindow{snap.Primary, snap.Secondary} {
				used, minutes, resets := db.RateLimitWindowColumns(w)
				var resetsAt any
				if text, ok := resets.(string); ok {
					if resetsAt, err = duckRateLimitTime(text); err != nil {
						return err
					}
				}
				args = append(args, used, minutes, resetsAt)
			}
		}
		b.WriteString(` ON CONFLICT DO NOTHING`)
		if err := s.execMutation(ctx, tx, b.String(), args...); err != nil {
			return fmt.Errorf("bulk inserting duckdb rate_limit_snapshots: %w", err)
		}
	}
	return nil
}

// GetLatestRateLimits returns the most recent rate-limit report for each
// agent, account, and limit, with the same selection as the SQLite archive
// (db.LatestRateLimitSnapshotsSQL).
func (s *Store) GetLatestRateLimits(ctx context.Context) ([]db.RateLimitSnapshot, error) {
	rows, err := s.queryContext(ctx, db.LatestRateLimitSnapshotsSQL)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb latest rate limits: %w", err)
	}
	defer rows.Close()
	out := []db.RateLimitSnapshot{}
	for rows.Next() {
		var snap db.RateLimitSnapshot
		var observed time.Time
		var pUsed, sUsed sql.NullFloat64
		var pMinutes, sMinutes sql.NullInt64
		var pResets, sResets sql.NullTime
		if err := rows.Scan(
			&snap.Agent, &snap.AccountKey, &snap.LimitID, &snap.PlanType,
			&snap.SessionID, &observed,
			&pUsed, &pMinutes, &pResets, &sUsed, &sMinutes, &sResets,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb rate limit snapshot: %w", err)
		}
		snap.ObservedAt = db.FormatRateLimitTime(observed)
		snap.Primary = db.RateLimitWindowFromColumns(pUsed, pMinutes, nullRateLimitTime(pResets))
		snap.Secondary = db.RateLimitWindowFromColumns(sUsed, sMinutes, nullRateLimitTime(sResets))
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb rate limit snapshots: %w", err)
	}
	return out, nil
}

func nullRateLimitTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return db.FormatRateLimitTime(t.Time)
}
