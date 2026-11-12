package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// syncRateLimitSnapshots appends the rate-limit reports the mirror has not
// consumed yet, tracked by a per-archive high-water id in sync_metadata.
// A rebuilt archive renumbers its rows and forces a full push, so a full push
// sends every row again; the sorting key makes rows the mirror holds replace
// themselves. Reports describe accounts, not projects, so project-filtered
// pushes skip them.
func (s *Sync) syncRateLimitSnapshots(ctx context.Context, full bool) error {
	if s.isFiltered() {
		return nil
	}
	key := s.archiveKey(rateLimitMaxIDKeyBase)
	stored, err := readMetadata(ctx, s.conn, key)
	if err != nil {
		return err
	}
	var sinceID int64
	if v := stored[key]; v != "" && !full {
		if sinceID, err = strconv.ParseInt(v, 10, 64); err != nil {
			return fmt.Errorf("parsing rate limit high-water id %q: %w", v, err)
		}
	}
	snaps, err := s.local.GetRateLimitSnapshots(ctx, sinceID)
	if err != nil {
		return fmt.Errorf("loading local rate limit snapshots: %w", err)
	}
	if len(snaps) == 0 {
		return nil
	}
	version := newPushVersion()
	rows := make([][]any, 0, len(snaps))
	maxID := sinceID
	for _, snap := range snaps {
		observed, ok := parseTimestamp(snap.ObservedAt)
		if !ok {
			return fmt.Errorf("parsing rate limit observed_at %q", snap.ObservedAt)
		}
		row := []any{
			db.SanitizeUTF8(snap.Agent), db.SanitizeUTF8(snap.AccountKey),
			db.SanitizeUTF8(snap.LimitID), db.SanitizeUTF8(snap.PlanType),
			snap.SessionID, observed,
		}
		for _, w := range []*db.RateLimitWindow{snap.Primary, snap.Secondary} {
			var used *float64
			var minutes *int64
			var resets *time.Time
			if w != nil {
				v := w.UsedPercent
				used = &v
				minutes = w.WindowMinutes
				if w.ResetsAt != "" {
					t, ok := parseTimestamp(w.ResetsAt)
					if !ok {
						return fmt.Errorf("parsing rate limit resets_at %q", w.ResetsAt)
					}
					resets = &t
				}
			}
			row = append(row, used, minutes, resets)
		}
		rows = append(rows, append(row, version))
		maxID = max(maxID, snap.ID)
	}
	if err := insertRows(ctx, s.conn, "rate_limit_snapshots", rows); err != nil {
		return err
	}
	return writeMetadata(ctx, s.conn, map[string]string{
		key: strconv.FormatInt(maxID, 10),
	})
}

// GetLatestRateLimits returns the most recent rate-limit report for each
// agent, account, and limit, with the same selection as the SQLite archive
// (db.LatestRateLimitSnapshotsSQL). A row pushed twice before a merge is an
// identical copy, so either copy can win the ranking.
func (s *Store) GetLatestRateLimits(ctx context.Context) ([]db.RateLimitSnapshot, error) {
	rows, err := s.queryContext(ctx, db.LatestRateLimitSnapshotsSQL)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse latest rate limits: %w", err)
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
			return nil, fmt.Errorf("scanning clickhouse rate limit snapshot: %w", err)
		}
		snap.ObservedAt = db.FormatRateLimitTime(observed)
		snap.Primary = db.RateLimitWindowFromColumns(pUsed, pMinutes, nullRateLimitTime(pResets))
		snap.Secondary = db.RateLimitWindowFromColumns(sUsed, sMinutes, nullRateLimitTime(sResets))
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse rate limit snapshots: %w", err)
	}
	return out, nil
}

func nullRateLimitTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return db.FormatRateLimitTime(t.Time)
}
