package postgres

import (
	"context"
	"fmt"
)

// Bump when IsFrustrationMarker changes a verdict.
const frustrationMarkerRulesVersion = 1

func frustrationSessionVersion(column string) string {
	return `(` + column + `::text || ':' || COALESCE(to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), ''))`
}

func (s *Store) loadStoredFrustrationCounts(ctx context.Context, ids []string, versions map[string]string) (map[string]int, error) {
	counts := make(map[string]int)
	rows, err := s.DB().QueryContext(ctx, `SELECT session_id, session_version, marker_count FROM session_frustration_counts WHERE session_id = ANY($1) AND rules_version = $2`, ids, frustrationMarkerRulesVersion)
	if isUndefinedTable(err) || isInsufficientPrivilege(err) {
		return counts, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, version string
		var count int
		if err := rows.Scan(&id, &version, &count); err != nil {
			return nil, err
		}
		if versions[id] == version {
			counts[id] = count
		}
	}
	return counts, rows.Err()
}

func (s *Store) storeFrustrationCounts(ctx context.Context, versions map[string]string, counts map[string]int) error {
	if len(versions) == 0 {
		return nil
	}
	ids := make([]string, 0, len(versions))
	tokens := make([]string, 0, len(versions))
	values := make([]int32, 0, len(versions))
	for id, version := range versions {
		ids = append(ids, id)
		tokens = append(tokens, version)
		values = append(values, int32(counts[id]))
	}
	_, err := s.DB().ExecContext(ctx, `INSERT INTO session_frustration_counts (session_id, session_version, rules_version, marker_count)
        SELECT id, version, $4, count FROM unnest($1::text[], $2::text[], $3::integer[]) AS t(id, version, count)
        ON CONFLICT (session_id) DO UPDATE SET session_version = EXCLUDED.session_version, rules_version = EXCLUDED.rules_version, marker_count = EXCLUDED.marker_count`, ids, tokens, values, frustrationMarkerRulesVersion)
	if err != nil {
		return fmt.Errorf("writing frustration counts: %w", err)
	}
	return nil
}
