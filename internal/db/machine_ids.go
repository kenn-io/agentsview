package db

import (
	"context"
	"slices"
)

// MatchingMachineIDs returns stored identities that exactly match candidates.
// Queries are deduplicated and chunked to stay within backend parameter limits.
func MatchingMachineIDs(ctx context.Context, reader messageRowsQuerier, dialect QueryDialect, candidates []string,
) ([]string, error) {
	candidates = slices.Clone(candidates)
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)
	var ids []string
	err := queryChunked(candidates, func(chunk []string) error {
		b := NewQueryBuilder(dialect, 0)
		predicate := b.ValuesPredicate("machine", chunk, true)
		rows, err := reader.QueryContext(ctx, "SELECT DISTINCT machine FROM sessions WHERE "+predicate+" ORDER BY machine", b.Args()...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// GetMatchingMachineIDs includes matching identities from trashed and orphaned sessions.
func (db *DB) GetMatchingMachineIDs(ctx context.Context, candidates []string) ([]string, error) {
	return MatchingMachineIDs(ctx, db.getReader(), SQLiteQueryDialect(), candidates)
}
