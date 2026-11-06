package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
)

// Deferred triggers take the shared counter lock only after the batch's writes.
// Its lock remains held through commit, so revisions follow commit order.
const usageChangeFunctionBodyPG = `
DECLARE
    revision_key text := 'agentsview.usage_change_revision_' || md5(TG_TABLE_SCHEMA);
    revision bigint := nullif(current_setting(revision_key, true), '')::bigint;
    session_key text;
    old_session_key text;
BEGIN
    IF revision IS NULL THEN
        UPDATE sync_metadata SET value = (value::bigint + 1)::text
        WHERE key = 'usage_change_revision' RETURNING value::bigint INTO revision;
        PERFORM set_config(revision_key, revision::text, true);
    END IF;
    IF TG_NARGS > 0 THEN
        IF TG_OP != 'DELETE' THEN
            session_key := to_jsonb(NEW)->>TG_ARGV[0];
        END IF;
        IF TG_OP != 'INSERT' THEN
            old_session_key := to_jsonb(OLD)->>TG_ARGV[0];
        END IF;
        IF session_key IS NOT NULL THEN
            INSERT INTO usage_session_changes(session_id, revision) VALUES(session_key, revision)
            ON CONFLICT(session_id) DO UPDATE SET revision = excluded.revision;
        END IF;
        IF old_session_key IS NOT NULL AND old_session_key IS DISTINCT FROM session_key THEN
            INSERT INTO usage_session_changes(session_id, revision) VALUES(old_session_key, revision)
            ON CONFLICT(session_id) DO UPDATE SET revision = excluded.revision;
        END IF;
    END IF;
    RETURN NULL;
END;
`

var usageChangeTablesPG = []struct{ table, sessionColumn string }{
	{"sessions", "id"}, {"usage_events", "session_id"}, {"session_plan_usage", "session_id"},
	{"model_pricing", ""}, {"model_pricing_bands", ""}, {"genai_pricing", ""}, {"cursor_usage_events", ""},
	{"source_project_identity_observations", ""}, {"source_project_identity_observation_scopes", ""},
	{"source_session_project_identity_snapshots", ""}, {"source_session_project_identity_snapshot_scopes", ""},
	{"source_worktree_project_mappings", ""}, {"source_worktree_project_mapping_scopes", ""},
}

func usageChangeJournalCurrentPG(ctx context.Context, q hostedQuerier) (bool, error) {
	var current bool
	err := q.QueryRowContext(ctx, `SELECT
    to_regclass('usage_session_changes') IS NOT NULL
    AND EXISTS (SELECT 1 FROM sync_metadata WHERE key = 'usage_change_revision')
    AND EXISTS (SELECT 1 FROM pg_proc WHERE oid = to_regprocedure('mark_usage_change()') AND prosrc = $1)
    AND (SELECT count(*) FROM pg_trigger WHERE tgfoid = to_regprocedure('mark_usage_change()')
         AND tgenabled = 'O' AND tgdeferrable AND tginitdeferred) = $2`,
		usageChangeFunctionBodyPG, len(usageChangeTablesPG)*3).Scan(&current)
	return current, err
}

func ensureUsageChangeJournalPG(ctx context.Context, pg *sql.DB) error {
	column, err := sessionVersionColumnPG(ctx, pg)
	if err != nil || column != "xmin" {
		return err
	}
	current, err := usageChangeJournalCurrentPG(ctx, pg)
	if err != nil || current {
		return err
	}
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	err = installUsageChangeJournalPG(ctx, tx)
	if isInsufficientPrivilege(err) {
		log.Printf("pg schema: usage change journal skipped, insufficient privilege: %v", err)
		return nil
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func installUsageChangeJournalPG(ctx context.Context, tx *sql.Tx) error {
	if err := lockSyncMetadataRow(ctx, tx, "usage_change_journal_lock"); err != nil {
		return err
	}
	current, err := usageChangeJournalCurrentPG(ctx, tx)
	if err != nil || current {
		return err
	}
	var schema string
	if err := tx.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return err
	}
	quoted, err := quoteIdentifier(schema)
	if err != nil {
		return err
	}
	ddl := `CREATE TABLE IF NOT EXISTS usage_session_changes(session_id TEXT PRIMARY KEY, revision BIGINT NOT NULL);
 CREATE INDEX IF NOT EXISTS idx_usage_session_changes_revision ON usage_session_changes(revision);
 INSERT INTO sync_metadata(key, value) VALUES('usage_change_revision', (extract(epoch FROM clock_timestamp()) * 1000000)::bigint::text) ON CONFLICT(key) DO NOTHING;
 CREATE OR REPLACE FUNCTION mark_usage_change() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
 SET search_path = pg_catalog, ` + quoted + `, pg_temp AS $journal$` + usageChangeFunctionBodyPG + `$journal$;
 REVOKE ALL ON FUNCTION mark_usage_change() FROM PUBLIC;
 `
	var triggers strings.Builder
	for _, table := range usageChangeTablesPG {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := "trg_uc_" + table.table + "_" + strings.ToLower(event)
			condition := ""
			if event == "UPDATE" {
				condition = "WHEN (OLD.* IS DISTINCT FROM NEW.*)"
			}
			argument := ""
			if table.sessionColumn != "" {
				argument = "'" + table.sessionColumn + "'"
			}
			fmt.Fprintf(&triggers, "DROP TRIGGER IF EXISTS %s ON %s; CREATE CONSTRAINT TRIGGER %s AFTER %s ON %s DEFERRABLE INITIALLY DEFERRED FOR EACH ROW %s EXECUTE FUNCTION mark_usage_change(%s);\n", name, table.table, name, event, table.table, condition, argument)
		}
	}
	_, err = tx.ExecContext(ctx, ddl+triggers.String())
	return err
}

// UsageChangeRevision returns 0 when this archive has no journal.
func (s *Store) UsageChangeRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := s.pg.QueryRowContext(ctx, `SELECT value::bigint FROM sync_metadata WHERE key = 'usage_change_revision'`).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

// LoadUsageChanges includes hard-deleted sessions whose latest change falls in
// (afterRevision, throughRevision]. Global changes only advance the revision.
func (s *Store) LoadUsageChanges(ctx context.Context, afterRevision, throughRevision int64) ([]string, error) {
	if afterRevision > throughRevision {
		return nil, fmt.Errorf("usage change window: after revision exceeds through revision")
	}
	rows, err := s.pg.QueryContext(ctx, `SELECT session_id FROM usage_session_changes WHERE revision > $1 AND revision <= $2 ORDER BY revision, session_id`, afterRevision, throughRevision)
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
