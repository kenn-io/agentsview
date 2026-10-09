package duckdb

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// RecentEdits returns files ordered by most-recent edit across all sessions,
// grouped by (project, file_path), with up to MaxEditsPerFile recent edits
// inlined per file. Trashed sessions are excluded. Delegates grouping and
// pagination to db.ScanRecentEdits.
func (s *Store) RecentEdits(
	ctx context.Context, p db.RecentEditsParams,
) (db.RecentEditsResult, error) {
	p = db.NormalizeRecentEditsParams(p)
	b := db.NewQueryBuilder(db.DuckDBQueryDialect(), 0)
	query := db.BuildRecentEditsQuery(p, b, "m.session_id = tc.session_id AND m.id = tc.message_id")
	rows, err := s.queryContext(ctx, query, b.Args()...)
	if err != nil {
		return db.RecentEditsResult{}, fmt.Errorf("querying duckdb recent edits: %w", err)
	}
	defer rows.Close()
	return db.ScanRecentEdits(rows, p)
}
