package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// Bump when the Go or PostgreSQL frustration classifier changes a verdict.
const frustrationMarkerRulesVersion = 1

func frustrationSessionVersion(column string) string {
	return `(` + column + `::text || ':' || COALESCE(to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), ''))`
}

func ensureFrustrationCountsPG(ctx context.Context, pg *sql.DB) {
	_, err := pg.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS session_frustration_counts (
        session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
        session_version TEXT NOT NULL,
        rules_version INTEGER NOT NULL,
        marker_count INTEGER NOT NULL
    )`)
	if err != nil && !IsReadOnlyError(err) {
		log.Printf("pg schema: frustration counts setup failed: %v", err)
	}
}

func (s *Store) loadStoredFrustrationCounts(ctx context.Context, ids []string, versions map[string]string, versionColumn string) (map[string]int, error) {
	counts := make(map[string]int)
	rows, err := s.DB().QueryContext(ctx, `SELECT c.session_id, c.session_version, c.marker_count FROM session_frustration_counts c JOIN sessions s ON s.id = c.session_id WHERE c.session_id = ANY($1) AND c.rules_version = $2 AND c.session_version = `+frustrationSessionVersion("s."+versionColumn), ids, frustrationMarkerRulesVersion)
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

func (s *Store) storeFrustrationCounts(ctx context.Context, versions map[string]string, counts map[string]int, versionColumn string) error {
	ids := make([]string, 0, len(versions))
	tokens := make([]string, 0, len(versions))
	values := make([]int32, 0, len(versions))
	for id, version := range versions {
		ids = append(ids, id)
		tokens = append(tokens, version)
		values = append(values, int32(counts[id]))
	}
	_, err := s.DB().ExecContext(ctx, `INSERT INTO session_frustration_counts (session_id, session_version, rules_version, marker_count)
        SELECT t.id, t.version, $4, t.count FROM unnest($1::text[], $2::text[], $3::integer[]) AS t(id, version, count)
        JOIN sessions s ON s.id = t.id AND `+frustrationSessionVersion("s."+versionColumn)+` = t.version
        ON CONFLICT (session_id) DO UPDATE SET session_version = EXCLUDED.session_version, rules_version = EXCLUDED.rules_version, marker_count = EXCLUDED.marker_count
        WHERE session_frustration_counts.rules_version <= EXCLUDED.rules_version`, ids, tokens, values, frustrationMarkerRulesVersion)
	if err != nil {
		return fmt.Errorf("writing frustration counts: %w", err)
	}
	return nil
}
