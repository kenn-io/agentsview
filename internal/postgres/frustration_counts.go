package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/signals"
)

// Bump when the Go or PostgreSQL frustration classifier changes a verdict.
const frustrationMarkerRulesVersion = 1

func frustrationSessionVersion(column string) string {
	return `(` + column + `::text || ':' || COALESCE(to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), ''))`
}

// ensureFrustrationCountsPG reports false when setup failed.
func ensureFrustrationCountsPG(ctx context.Context, pg *sql.DB) bool {
	_, err := pg.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS session_frustration_counts (
        session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
        session_version TEXT NOT NULL,
        rules_version INTEGER NOT NULL,
        marker_count INTEGER NOT NULL
    )`)
	if err != nil && !IsReadOnlyError(err) {
		log.Printf("pg schema: frustration counts setup failed: %v", err)
	}
	return err == nil
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

// Reserve request time when another writer holds a count row.
func (s *Store) storeFrustrationCountsForRead(ctx context.Context, versions map[string]string, counts map[string]int, versionColumn string) error {
	budget := time.Second
	if deadline, ok := frustrationRequestDeadline(ctx); ok {
		budget = min(budget, time.Until(deadline)/2)
	}
	writeCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return s.storeFrustrationCounts(writeCtx, versions, counts, versionColumn)
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
        ORDER BY t.id
        ON CONFLICT (session_id) DO UPDATE SET session_version = EXCLUDED.session_version, rules_version = EXCLUDED.rules_version, marker_count = EXCLUDED.marker_count
        WHERE session_frustration_counts.rules_version <= EXCLUDED.rules_version`, ids, tokens, values, frustrationMarkerRulesVersion)
	if err != nil {
		return fmt.Errorf("writing frustration counts: %w", err)
	}
	return nil
}

func (s *Store) scanFrustrationCounts(ctx context.Context, ids []string, versionColumn string) (map[string]int, map[string]string, error) {
	var encoding string
	if err := s.pg.QueryRowContext(ctx, `SELECT current_setting('server_encoding')`).Scan(&encoding); err != nil {
		return nil, nil, fmt.Errorf("reading server encoding: %w", err)
	}
	counts := make(map[string]int, len(ids))
	versions := make(map[string]string, len(ids))
	query, args := pgFrustrationQuery(encoding, frustrationSessionVersion(versionColumn), ids)
	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("querying frustration markers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, content string
		var version *string
		var decided bool
		if err := rows.Scan(&id, &version, &decided, &content); err != nil {
			return nil, nil, fmt.Errorf("scanning frustration marker: %w", err)
		}
		if version != nil {
			versions[id] = *version
			continue
		}
		if decided || signals.IsFrustrationMarker(content) {
			counts[id]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating frustration markers: %w", err)
	}
	return counts, versions, nil
}

func (s *Store) staleFrustrationSessions(ctx context.Context, versionColumn string) ([]string, []string, []string, []string, error) {
	where := db.BuildAnalyticsWhere(db.AnalyticsFilter{}, db.NewQueryBuilder(s.sessionDialect(), 1), "s.", "", nil)
	rows, err := s.pg.QueryContext(ctx, `SELECT s.id, COALESCE(NOT s.is_automated AND COALESCE(s.started_at, s.created_at) >= (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '364 days') AT TIME ZONE 'UTC' AND COALESCE(s.started_at, s.created_at) < (date_trunc('day', now() AT TIME ZONE 'UTC') + interval '1 day') AT TIME ZONE 'UTC', false), COALESCE(NOT s.is_automated AND COALESCE(s.started_at, s.created_at) >= (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '365 days') AT TIME ZONE 'UTC' AND COALESCE(s.started_at, s.created_at) < (date_trunc('day', now() AT TIME ZONE 'UTC') - interval '364 days') AT TIME ZONE 'UTC', false), COALESCE(NOT s.is_automated AND COALESCE(s.started_at, s.created_at) >= (date_trunc('day', now() AT TIME ZONE 'UTC') + interval '1 day') AT TIME ZONE 'UTC' AND COALESCE(s.started_at, s.created_at) < (date_trunc('day', now() AT TIME ZONE 'UTC') + interval '2 days') AT TIME ZONE 'UTC', false) FROM sessions s LEFT JOIN session_frustration_counts c ON c.session_id=s.id WHERE `+where+` AND (c.session_id IS NULL OR (c.rules_version <= $1 AND (c.rules_version < $1 OR c.session_version <> `+frustrationSessionVersion("s."+versionColumn)+`))) ORDER BY s.id`, frustrationMarkerRulesVersion)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer rows.Close()
	var dash, edge, next, rest []string
	for rows.Next() {
		var id string
		var inDashboard, inEdge, inNext bool
		if err := rows.Scan(&id, &inDashboard, &inEdge, &inNext); err != nil {
			return nil, nil, nil, nil, err
		}
		if inDashboard {
			dash = append(dash, id)
		} else if inEdge {
			edge = append(edge, id)
		} else if inNext {
			next = append(next, id)
		} else {
			rest = append(rest, id)
		}
	}
	return dash, edge, next, rest, rows.Err()
}

func (s *Store) canFillFrustrationCounts(ctx context.Context) (bool, error) {
	var writable bool
	err := s.pg.QueryRowContext(ctx, `SELECT COALESCE(NOT pg_is_in_recovery() AND current_setting('transaction_read_only') = 'off' AND has_table_privilege(to_regclass('session_frustration_counts'), 'SELECT') AND has_table_privilege(to_regclass('session_frustration_counts'), 'INSERT') AND has_table_privilege(to_regclass('session_frustration_counts'), 'UPDATE'), false)`).Scan(&writable)
	return writable, err
}
