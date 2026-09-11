package postgres

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

// LatestRateLimitSnapshots and RateLimitSnapshotHistory are SQLite-only
// (see docs/agents/storage.md): rate_limit_snapshots is a local
// vendor-data table, not part of the SQLite/PostgreSQL/DuckDB parity
// contract. The PostgreSQL reader returns an empty result rather than an
// error so the Usage page's rate-limits section simply stays hidden when
// PostgreSQL is the active read backend.

// LatestRateLimitSnapshots is not supported by the PostgreSQL backend.
func (s *Store) LatestRateLimitSnapshots(
	_ context.Context, _ db.RateLimitFilter,
) ([]db.RateLimitSnapshot, error) {
	return nil, nil
}

// RateLimitSnapshotHistory is not supported by the PostgreSQL backend.
func (s *Store) RateLimitSnapshotHistory(
	_ context.Context, _ db.RateLimitHistoryFilter,
) ([]db.RateLimitSnapshot, error) {
	return nil, nil
}
