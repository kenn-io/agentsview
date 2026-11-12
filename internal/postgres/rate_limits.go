package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// rateLimitSnapshotsDDL mirrors SQLite's rate_limit_snapshots: the
// subscription rate-limit reports agents record, which describe accounts and
// carry no foreign key to sessions.
const rateLimitSnapshotsDDL = `
CREATE TABLE IF NOT EXISTS rate_limit_snapshots (
    id BIGSERIAL PRIMARY KEY,
    agent TEXT NOT NULL,
    account_key TEXT NOT NULL DEFAULT '',
    limit_id TEXT NOT NULL DEFAULT '',
    plan_type TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    primary_used_percent DOUBLE PRECISION,
    primary_window_minutes BIGINT,
    primary_resets_at TIMESTAMPTZ,
    secondary_used_percent DOUBLE PRECISION,
    secondary_window_minutes BIGINT,
    secondary_resets_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_rate_limit_snapshots_identity
    ON rate_limit_snapshots (session_id, limit_id, observed_at);

CREATE INDEX IF NOT EXISTS idx_rate_limit_snapshots_latest
    ON rate_limit_snapshots (agent, account_key, limit_id, observed_at);
`

// rateLimitHighWaterKeyPrefix, followed by the source archive id, names the
// sync_metadata row recording the largest local rate_limit_snapshots id this
// target holds, as "<id>:<database generation>".
const rateLimitHighWaterKeyPrefix = "rate_limit_max_id:"

// syncRateLimitSnapshots sends the rate-limit reports the target has not
// received from this archive yet, the same way syncCursorUsageEvents sends
// account usage rows: local ids only grow within one database generation, so
// a stored id from another generation, or a full push, sends every row again,
// and the identity index makes rows the target already holds no-ops.
func (s *Sync) syncRateLimitSnapshots(ctx context.Context, full bool) error {
	// Reports describe accounts, not projects, so a project-filtered push
	// cannot say which of them belong to it.
	if s.isFiltered() {
		return nil
	}
	localMax, err := s.local.MaxRateLimitSnapshotID(ctx)
	if err != nil {
		return err
	}
	if localMax == 0 {
		return nil
	}
	key := rateLimitHighWaterKeyPrefix + s.archiveID
	var sinceID int64
	if !full {
		var stored string
		err = s.pg.QueryRowContext(ctx,
			`SELECT value FROM sync_metadata WHERE key = $1`, key).Scan(&stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("reading rate limit high-water id: %w", err)
		}
		if id, generation, ok := strings.Cut(stored, ":"); ok && generation == s.databaseGeneration {
			if sinceID, err = strconv.ParseInt(id, 10, 64); err != nil {
				return fmt.Errorf("parsing rate limit high-water id %q: %w", stored, err)
			}
		}
	}
	if sinceID >= localMax {
		return nil
	}
	snaps, err := s.local.GetRateLimitSnapshots(ctx, sinceID)
	if err != nil {
		return fmt.Errorf("loading local rate limit snapshots: %w", err)
	}
	if len(snaps) == 0 {
		return nil
	}
	maxID := sinceID
	for _, snap := range snaps {
		maxID = max(maxID, snap.ID)
	}

	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning rate limit sync tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := bulkInsertRateLimitSnapshots(ctx, tx, snaps); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sync_metadata (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, strconv.FormatInt(maxID, 10)+":"+s.databaseGeneration,
	); err != nil {
		return fmt.Errorf("recording rate limit high-water id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing pg rate limit sync: %w", err)
	}
	return nil
}

// pgRateLimitTime converts a stored rate-limit time to a timestamptz value,
// or NULL when it is empty.
func pgRateLimitTime(value string) (any, error) {
	if value == "" {
		return nil, nil
	}
	t, ok := ParseSQLiteTimestamp(value)
	if !ok {
		return nil, fmt.Errorf("parsing rate limit time %q", value)
	}
	return t, nil
}

func bulkInsertRateLimitSnapshots(
	ctx context.Context, tx *sql.Tx, snaps []db.RateLimitSnapshot,
) error {
	const batchSize = 100
	const columns = 12
	for i := 0; i < len(snaps); i += batchSize {
		batch := snaps[i:min(i+batchSize, len(snaps))]
		var b strings.Builder
		b.WriteString(`INSERT INTO rate_limit_snapshots (
			agent, account_key, limit_id, plan_type, session_id, observed_at,
			primary_used_percent, primary_window_minutes, primary_resets_at,
			secondary_used_percent, secondary_window_minutes, secondary_resets_at
		) VALUES `)
		args := make([]any, 0, len(batch)*columns)
		for j, snap := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('(')
			for c := range columns {
				if c > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, "$%d", j*columns+c+1)
			}
			b.WriteByte(')')
			observed, err := pgRateLimitTime(snap.ObservedAt)
			if err != nil {
				return err
			}
			if observed == nil {
				return fmt.Errorf("rate limit snapshot %d has no observed time", snap.ID)
			}
			args = append(args,
				sanitizePG(snap.Agent), sanitizePG(snap.AccountKey),
				sanitizePG(snap.LimitID), sanitizePG(snap.PlanType),
				sanitizePG(snap.SessionID), observed,
			)
			for _, w := range []*db.RateLimitWindow{snap.Primary, snap.Secondary} {
				used, minutes, resets := db.RateLimitWindowColumns(w)
				var resetsAt any
				if s, ok := resets.(string); ok {
					if resetsAt, err = pgRateLimitTime(s); err != nil {
						return err
					}
				}
				args = append(args, used, minutes, resetsAt)
			}
		}
		b.WriteString(` ON CONFLICT (session_id, limit_id, observed_at) DO NOTHING`)
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return fmt.Errorf("bulk inserting rate_limit_snapshots: %w", err)
		}
	}
	return nil
}

// GetLatestRateLimits returns the most recent rate-limit report for each
// agent, account, and limit, with the same selection as the SQLite archive
// (db.LatestRateLimitSnapshotsSQL). A schema no push has upgraded yet has no
// table and no reports.
func (s *Store) GetLatestRateLimits(ctx context.Context) ([]db.RateLimitSnapshot, error) {
	return pgLatestRateLimits(ctx, s.pg)
}

func pgLatestRateLimits(ctx context.Context, pg hostedQuerier) ([]db.RateLimitSnapshot, error) {
	if !pgHasTable(ctx, pg, "rate_limit_snapshots") {
		return []db.RateLimitSnapshot{}, nil
	}
	rows, err := pg.QueryContext(ctx, db.LatestRateLimitSnapshotsSQL)
	if err != nil {
		return nil, fmt.Errorf("querying pg latest rate limits: %w", err)
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
			return nil, fmt.Errorf("scanning pg rate limit snapshot: %w", err)
		}
		snap.ObservedAt = db.FormatRateLimitTime(observed)
		snap.Primary = db.RateLimitWindowFromColumns(pUsed, pMinutes, formatNullRateLimitTime(pResets))
		snap.Secondary = db.RateLimitWindowFromColumns(sUsed, sMinutes, formatNullRateLimitTime(sResets))
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pg rate limit snapshots: %w", err)
	}
	return out, nil
}

func formatNullRateLimitTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return db.FormatRateLimitTime(t.Time)
}

// GetLatestRateLimits returns no reports: hosted tenants are built from raw
// uploads, and raw derivation does not produce rate-limit reports.
func (h *HostedStore) GetLatestRateLimits(context.Context) ([]db.RateLimitSnapshot, error) {
	return []db.RateLimitSnapshot{}, nil
}
