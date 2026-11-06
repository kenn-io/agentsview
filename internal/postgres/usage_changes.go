package postgres

import (
	"context"
	"database/sql"
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

const usageChangeJournalVersionPG = 1

var usageChangeTablesPG = []struct{ table, sessionColumn string }{
	{"sessions", "id"},
	{"model_pricing", ""}, {"model_pricing_bands", ""}, {"genai_pricing", ""}, {"cursor_usage_events", ""},
	{"source_project_identity_observations", ""}, {"source_project_identity_observation_scopes", ""},
	{"source_session_project_identity_snapshots", ""}, {"source_session_project_identity_snapshot_scopes", ""},
	{"source_worktree_project_mappings", ""}, {"source_worktree_project_mapping_scopes", ""},
}

func usageChangeJournalCurrentPG(ctx context.Context, q hostedQuerier) (bool, error) {
	var current bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM sync_metadata WHERE key = 'usage_change_journal_version' AND value::bigint >= $1
)`, usageChangeJournalVersionPG).Scan(&current)
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
			if table.table == "model_pricing" || table.table == "model_pricing_bands" {
				const oldModel = `OLD.model_pattern NOT LIKE '\_%'`
				const newModel = `NEW.model_pattern NOT LIKE '\_%'`
				switch event {
				case "INSERT":
					condition = newModel
				case "DELETE":
					condition = oldModel
				case "UPDATE":
					condition = "(" + oldModel + " OR " + newModel + ") AND "
				}
			}
			if event == "UPDATE" {
				condition += "OLD.* IS DISTINCT FROM NEW.*"
			}
			if condition != "" {
				condition = "WHEN (" + condition + ")"
			}
			argument := ""
			if table.sessionColumn != "" {
				argument = "'" + table.sessionColumn + "'"
			}
			fmt.Fprintf(&triggers, `DO $trigger$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = '%s'::regclass AND tgname = '%s') THEN
 CREATE CONSTRAINT TRIGGER %s AFTER %s ON %s DEFERRABLE INITIALLY DEFERRED FOR EACH ROW %s EXECUTE FUNCTION mark_usage_change(%s);
 END IF;
 END $trigger$;
`, table.table, name, name, event, table.table, condition, argument)
		}
	}
	if _, err = tx.ExecContext(ctx, ddl+triggers.String()+`
 DO $grants$ DECLARE reader text; BEGIN
 FOR reader IN
     SELECT DISTINCT CASE WHEN acl.grantee = 0 THEN 'PUBLIC' ELSE quote_ident(pg_get_userbyid(acl.grantee)) END
     FROM pg_class c, LATERAL aclexplode(coalesce(c.relacl, acldefault('r', c.relowner))) acl
     WHERE c.oid = 'sessions'::regclass AND acl.privilege_type = 'SELECT'
 LOOP
     EXECUTE 'GRANT SELECT ON usage_session_changes TO ' || reader;
 END LOOP;
 END $grants$;`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_metadata(key,value) VALUES('usage_change_journal_version',$1)
 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(usageChangeJournalVersionPG))
	return err
}

const usageChangeRevisionQueryPG = `SELECT coalesce((
 SELECT value::bigint FROM sync_metadata WHERE key = 'usage_change_revision'
 AND EXISTS (SELECT 1 FROM sync_metadata WHERE key = 'usage_change_journal_version' AND value::bigint = $1)
), 0) AS revision`

// UsageChangeRevision returns 0 when the journal is absent or its version is unsupported.
func (s *Store) UsageChangeRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := s.pg.QueryRowContext(ctx, usageChangeRevisionQueryPG, usageChangeJournalVersionPG).Scan(&revision)
	return revision, err
}

// LoadUsageChanges returns the current revision and sessions changed after
// afterRevision from one snapshot, including hard-deleted sessions. Global
// changes only advance the revision. A write committed after this snapshot
// appears in the next call when the caller uses the returned revision.
func (s *Store) LoadUsageChanges(ctx context.Context, afterRevision int64) (int64, []string, error) {
	rows, err := s.pg.QueryContext(ctx, `SELECT r.revision, c.session_id
 FROM (`+usageChangeRevisionQueryPG+`) r
 LEFT JOIN usage_session_changes c ON r.revision > 0 AND c.revision > $2 AND c.revision <= r.revision
 ORDER BY c.revision, c.session_id`, usageChangeJournalVersionPG, afterRevision)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var revision int64
	var ids []string
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&revision, &id); err != nil {
			return 0, nil, err
		}
		if id.Valid {
			ids = append(ids, id.String)
		}
	}
	return revision, ids, rows.Err()
}
