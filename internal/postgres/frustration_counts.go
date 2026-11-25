package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/signals"
)

// Bump when the Go or PostgreSQL frustration classifier changes a verdict.
const frustrationMarkerRulesVersion = 1

func frustrationSessionVersion(column string) string {
	return `(` + column + `::text || ':' || COALESCE(to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), ''))`
}

func frustrationCountValid(column, rulesParam string) string {
	return `c.rules_version = ` + rulesParam + ` AND c.session_version = ` + frustrationSessionVersion("s."+column)
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
	rows, err := s.DB().QueryContext(ctx, `SELECT c.session_id, c.session_version, c.marker_count FROM session_frustration_counts c JOIN sessions s ON s.id = c.session_id WHERE c.session_id = ANY($1) AND `+frustrationCountValid(versionColumn, "$2"), ids, frustrationMarkerRulesVersion)
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

// Optional saves belong to the store and must not hold up the response.
func (s *Store) storeFrustrationCountsForRead(versions map[string]string, counts map[string]int, versionColumn string) {
	queue := &s.runtimeState().frustrationSaves
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.closed {
		return
	}
	if queue.ctx == nil {
		queue.ctx, queue.cancel = context.WithCancel(context.Background())
	}
	if queue.queued == nil {
		queue.queued = make(map[frustrationSaveKey]struct{})
	}
	save := frustrationSave{make(map[string]string), make(map[string]int), versionColumn}
	for id, version := range versions {
		key := frustrationSaveKey{id, version, versionColumn}
		if _, exists := queue.queued[key]; exists {
			continue
		}
		queue.queued[key] = struct{}{}
		save.versions[id] = version
		save.counts[id] = counts[id]
	}
	if len(save.versions) == 0 {
		return
	}
	queue.pending = append(queue.pending, save)
	if !queue.running {
		queue.running = true
		queue.wg.Go(func() { s.drainFrustrationSaves(queue) })
	}
}

func (s *Store) drainFrustrationSaves(queue *frustrationSaveQueue) {
	for {
		queue.mu.Lock()
		if queue.closed || len(queue.pending) == 0 {
			queue.running = false
			queue.pending = nil
			queue.queued = nil
			queue.mu.Unlock()
			return
		}
		save := queue.pending[0]
		queue.pending[0] = frustrationSave{}
		queue.pending = queue.pending[1:]
		queue.mu.Unlock()
		// Saves outlive requests but stop when the store closes.
		ctx, cancel := context.WithTimeout(queue.ctx, frustrationFillChunkTimeout)
		err := s.storeFrustrationCountsWithLockTimeout(ctx, save.versions, save.counts, save.column)
		pgErr, isPGError := errors.AsType[*pgconn.PgError](err)
		retry := err != nil && (ctx.Err() == context.DeadlineExceeded || (isPGError && pgErr.Code == "55P03"))
		cancel()
		if err != nil && !retry && !isUndefinedTable(err) && !IsReadOnlyError(err) && queue.ctx.Err() == nil {
			log.Printf("storing frustration counts: %v", err)
		}
		queue.mu.Lock()
		if retry && !queue.closed {
			queue.pending = append(queue.pending, save)
		} else {
			for id, version := range save.versions {
				delete(queue.queued, frustrationSaveKey{id, version, save.column})
			}
		}
		queue.mu.Unlock()
		if retry {
			// Give foreground reads a chance to acquire the connection before retrying.
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-timer.C:
			case <-queue.ctx.Done():
			}
			timer.Stop()
		}
	}
}

func (s *Store) storeFrustrationCountsWithLockTimeout(ctx context.Context, versions map[string]string, counts map[string]int, column string) error {
	if s.cockroach {
		return s.storeFrustrationCounts(ctx, versions, counts, column)
	}
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '100ms'`); err != nil {
		return err
	}
	if err := writeFrustrationCounts(ctx, tx, versions, counts, column); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) storeFrustrationCounts(ctx context.Context, versions map[string]string, counts map[string]int, versionColumn string) error {
	return writeFrustrationCounts(ctx, s.DB(), versions, counts, versionColumn)
}

func writeFrustrationCounts(ctx context.Context, pg interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, versions map[string]string, counts map[string]int, versionColumn string) error {
	ids := make([]string, 0, len(versions))
	tokens := make([]string, 0, len(versions))
	values := make([]int32, 0, len(versions))
	for id, version := range versions {
		ids = append(ids, id)
		tokens = append(tokens, version)
		values = append(values, int32(counts[id]))
	}
	_, err := pg.ExecContext(ctx, `INSERT INTO session_frustration_counts (session_id, session_version, rules_version, marker_count)
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

// staleFrustrationSessions lists stale sessions in Dashboard order.
func (s *Store) staleFrustrationSessions(ctx context.Context, versionColumn string) ([]string, error) {
	where := db.BuildAnalyticsWhere(db.AnalyticsFilter{}, db.NewQueryBuilder(s.sessionDialect(), 1), "s.", "", nil)
	rows, err := s.pg.QueryContext(ctx, `SELECT s.id FROM sessions s LEFT JOIN session_frustration_counts c ON c.session_id=s.id WHERE `+where+` AND (c.session_id IS NULL OR (c.rules_version <= $1 AND NOT (`+frustrationCountValid(versionColumn, "$1")+`))) ORDER BY s.is_automated, COALESCE(s.started_at, s.created_at) DESC NULLS LAST, s.id`, frustrationMarkerRulesVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) canFillFrustrationCounts(ctx context.Context) (bool, error) {
	var writable bool
	err := s.pg.QueryRowContext(ctx, `SELECT COALESCE(NOT pg_is_in_recovery() AND current_setting('transaction_read_only') = 'off' AND has_table_privilege(to_regclass('session_frustration_counts'), 'SELECT') AND has_table_privilege(to_regclass('session_frustration_counts'), 'INSERT') AND has_table_privilege(to_regclass('session_frustration_counts'), 'UPDATE'), false)`).Scan(&writable)
	return writable, err
}
