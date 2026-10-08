package postgres

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// RecentEdits returns files ordered by most-recent edit across all sessions,
// grouped by (project, file_path), with up to MaxEditsPerFile recent edits
// inlined per file. Trashed sessions are excluded.
func (s *Store) RecentEdits(
	ctx context.Context, p db.RecentEditsParams,
) (db.RecentEditsResult, error) {
	p = db.NormalizeRecentEditsParams(p)
	b := db.NewQueryBuilder(db.PostgresQueryDialect(), 0)
	query := db.BuildRecentEditsQuery(p, b, "m.session_id = tc.session_id AND m.ordinal = tc.message_ordinal")
	rows, err := s.pg.QueryContext(ctx, query, b.Args()...)
	if err != nil {
		return db.RecentEditsResult{}, fmt.Errorf("pg recent edits: %w", err)
	}
	defer rows.Close()
	return db.ScanRecentEdits(rows, p)
}
