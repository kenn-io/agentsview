package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"math"
	"time"

	"go.kenn.io/agentsview/internal/parser"
)

type RateLimitSeries struct {
	Machine string                     `json:"machine"`
	LimitID string                     `json:"limit_id"`
	Current parser.RateLimitSnapshot   `json:"current"`
	Points  []parser.RateLimitSnapshot `json:"points"`
}

const rateLimitBucketCount = 256

// Full parses replace the source's readings, even when the replacement is empty.
// source_id deliberately has no FK: archived readings survive session deletion.
func writeRateLimits(
	exec func(string, ...any) (sql.Result, error),
	sourceID, machine string, snapshots []parser.RateLimitSnapshot, replace bool,
) error {
	if snapshots == nil {
		return nil
	}
	if replace {
		if _, err := exec(`DELETE FROM rate_limit_snapshots
			WHERE vendor = 'codex' AND source_id = ?`, sourceID); err != nil {
			return err
		}
	}
	for _, snapshot := range snapshots {
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if _, err := exec(`INSERT OR REPLACE INTO rate_limit_snapshots
			(vendor, machine, source_id, ordinal, limit_id, observed_at, snapshot)
			VALUES ('codex', ?, ?, ?, ?, ?, ?)`, machine, sourceID,
			snapshot.Ordinal, snapshot.LimitID, snapshot.ObservedAt.UnixNano(), string(encoded)); err != nil {
			return err
		}
	}
	return nil
}

// RateLimits returns the latest reading and up to 512 observations in [since, until).
// Bounds are Unix seconds; zero leaves that end of history unbounded.
func (db *DB) RateLimits(ctx context.Context, machine string, since, until int64) ([]RateLimitSeries, error) {
	start, end := since*int64(time.Second), int64(math.MaxInt64)
	if until != 0 {
		end = until * int64(time.Second)
	}
	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'rate_limit_snapshots'
	)`).Scan(&exists); err != nil || !exists {
		return nil, err
	}
	// Seek past each key instead of scanning its accumulated history.
	query := `WITH RECURSIVE machines(name) AS (
		SELECT min(machine) FROM rate_limit_snapshots WHERE vendor = 'codex'
		UNION ALL
		SELECT (SELECT min(machine) FROM rate_limit_snapshots WHERE vendor = 'codex'
			AND machine > machines.name) FROM machines WHERE name IS NOT NULL
	), limits(machine, limit_id) AS (
		SELECT name, (SELECT min(limit_id) FROM rate_limit_snapshots WHERE vendor = 'codex'
			AND machine = machines.name) FROM machines WHERE name IS NOT NULL
		UNION ALL
		SELECT machine, (SELECT min(limit_id) FROM rate_limit_snapshots WHERE vendor = 'codex'
			AND machine = limits.machine AND limit_id > limits.limit_id) FROM limits WHERE limit_id IS NOT NULL
	) SELECT json_group_array(json_object('machine', machine, 'limit_id', limit_id)
		ORDER BY machine, limit_id) FROM limits WHERE limit_id IS NOT NULL`
	filter, args := sqliteAnalyticsCSVPredicate("machine", machine)
	if filter != "" {
		query += " AND " + filter
	}
	var raw string
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&raw); err != nil {
		return nil, err
	}
	var series []RateLimitSeries
	if err := json.Unmarshal([]byte(raw), &series); err != nil {
		return nil, err
	}
	for i := range series {
		item := &series[i]
		err := tx.QueryRowContext(ctx, `WITH RECURSIVE
			bounds(lo, hi) AS (SELECT
				(SELECT min(observed_at) FROM rate_limit_snapshots WHERE vendor = 'codex'
					AND machine = ?1 AND limit_id = ?2 AND observed_at >= ?3 AND observed_at < ?4),
				(SELECT max(observed_at) FROM rate_limit_snapshots WHERE vendor = 'codex'
					AND machine = ?1 AND limit_id = ?2 AND observed_at >= ?3 AND observed_at < ?4)),
			buckets(n, lo, hi, step) AS (
				SELECT 0, lo, hi, max(1, (hi - lo) / ?5) FROM bounds WHERE lo IS NOT NULL
				UNION ALL SELECT n + 1, lo, hi, step FROM buckets WHERE n < ?5 - 1 AND lo + (n + 1) * step <= hi),
			samples(n, edge, id) AS (
				SELECT -1, 0, (SELECT rowid FROM rate_limit_snapshots WHERE vendor = 'codex'
					AND machine = ?1 AND limit_id = ?2 ORDER BY observed_at DESC, source_id DESC, ordinal DESC LIMIT 1)
				UNION ALL
				SELECT n, 0, (SELECT rowid FROM rate_limit_snapshots WHERE vendor = 'codex'
					AND machine = ?1 AND limit_id = ?2 AND observed_at >= lo + n * step
					AND observed_at <= CASE WHEN n = ?5 - 1 THEN hi ELSE lo + (n + 1) * step - 1 END
					ORDER BY observed_at, source_id, ordinal LIMIT 1) FROM buckets
				UNION ALL
				SELECT n, 1, (SELECT rowid FROM rate_limit_snapshots WHERE vendor = 'codex'
					AND machine = ?1 AND limit_id = ?2 AND observed_at >= lo + n * step
					AND observed_at <= CASE WHEN n = ?5 - 1 THEN hi ELSE lo + (n + 1) * step - 1 END
					ORDER BY observed_at DESC, source_id DESC, ordinal DESC LIMIT 1) FROM buckets),
			ranked(n, edge, snapshot, rank) AS (
				SELECT s.n, s.edge, r.snapshot, row_number() OVER (
					PARTITION BY s.n, r.observed_at ORDER BY s.edge DESC)
				FROM samples s JOIN rate_limit_snapshots r ON r.rowid = s.id)
			SELECT json_object('machine', ?1, 'limit_id', ?2,
				'current', json(max(CASE WHEN n = -1 THEN snapshot END)),
				'points', json_group_array(json(snapshot) ORDER BY n, edge) FILTER (WHERE n >= 0))
			FROM ranked WHERE rank = 1`, item.Machine, item.LimitID, start, end, rateLimitBucketCount).Scan(&raw)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), item); err != nil {
			return nil, err
		}
	}
	return series, nil
}
