package db

import (
	"context"
	"slices"
	"strings"
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
		placeholders := make([]string, len(chunk))
		for i, candidate := range chunk {
			placeholders[i] = b.Add(candidate)
		}
		rows, err := reader.QueryContext(ctx, "SELECT DISTINCT machine FROM sessions WHERE machine IN ("+strings.Join(placeholders, ",")+") ORDER BY machine", b.Args()...)
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
