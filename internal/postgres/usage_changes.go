package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
)

// Statement triggers mark each changed session pending with revision 0 in one
// set-based write. The empty session ID marks pricing and identity changes.
const markUsageChangeFunctionBodyPG = `
BEGIN
    PERFORM set_config('agentsview.uc_stamped_' || 'usage_session_changes'::regclass::oid, '', true);
    IF TG_OP = 'DELETE' THEN
        INSERT INTO usage_session_changes(session_id, revision)
        SELECT DISTINCT %[1]s, 0 FROM old_rows WHERE %[2]s
        ON CONFLICT(session_id) DO UPDATE SET revision = 0;
    ELSIF TG_OP = 'UPDATE' THEN
        INSERT INTO usage_session_changes(session_id, revision)
        SELECT DISTINCT %[1]s, 0 FROM (
            (SELECT %[3]s FROM old_rows row_data EXCEPT SELECT %[3]s FROM new_rows row_data)
            UNION ALL (SELECT %[3]s FROM new_rows row_data EXCEPT SELECT %[3]s FROM old_rows row_data)
        ) changed WHERE %[2]s
        ON CONFLICT(session_id) DO UPDATE SET revision = 0;
    ELSE
        INSERT INTO usage_session_changes(session_id, revision)
        SELECT DISTINCT %[1]s, 0 FROM new_rows WHERE %[2]s
        ON CONFLICT(session_id) DO UPDATE SET revision = 0;
    END IF;
    RETURN NULL;
END;
`

// The deferred stamp takes the counter lock at commit and holds it through
// commit, so revisions follow commit order. A transaction-local flag lets later
// queued calls return without scanning dead index entries from the stamp update.
// Each mark clears it before writing, including after SET CONSTRAINTS IMMEDIATE.
// The table OID keeps flags separate when a transaction writes several schemas.
const stampUsageChangeFunctionBodyPG = `
DECLARE
    stamped bigint;
    stamp_key text := 'agentsview.uc_stamped_' || TG_RELID;
BEGIN
    IF current_setting(stamp_key, true) = 'on' THEN
        RETURN NULL;
    END IF;
    PERFORM set_config(stamp_key, 'on', true);
    IF NOT EXISTS (SELECT 1 FROM usage_session_changes WHERE revision = 0) THEN
        RETURN NULL;
    END IF;
    UPDATE sync_metadata SET value = (value::bigint + 1)::text
    WHERE key = 'usage_change_revision'
    RETURNING value::bigint INTO stamped;
    UPDATE usage_session_changes SET revision = stamped WHERE revision = 0;
    RETURN NULL;
END;
`

const usageChangeJournalVersionPG = 1

// Messages and plan usage reach the journal through the sessions.updated_at
// bump every released publisher writes with them.
var usageChangeTablesPG = []struct{ table, sessionColumn string }{
	{"sessions", "id"},
	{"usage_events", "session_id"},
	{"model_pricing", ""},
	{"model_pricing_bands", ""},
	{"genai_pricing", ""},
	{"cursor_usage_events", ""},
	{"source_archives", ""},
	{"source_project_identity_observations", ""},
	{"source_project_identity_observation_scopes", ""},
	{"source_session_project_identity_snapshots", ""},
	{"source_session_project_identity_snapshot_scopes", ""},
	{"source_worktree_project_mappings", ""},
	{"source_worktree_project_mapping_scopes", ""},
}

var dropUsageChangeJournalSQL = func() string {
	var ddl strings.Builder
	ddl.WriteString(`DROP TABLE IF EXISTS usage_session_changes;
 DROP FUNCTION IF EXISTS stamp_usage_change();
 DO $cleanup$ BEGIN
 IF to_regclass('sync_metadata') IS NOT NULL THEN
     DELETE FROM sync_metadata WHERE key IN ('usage_change_revision', 'usage_change_journal_lock', 'usage_change_journal_version');
 END IF;
 END $cleanup$;`)
	for _, table := range usageChangeTablesPG {
		ddl.WriteString("DROP FUNCTION IF EXISTS mark_usage_" + table.table + "() CASCADE;")
	}
	return ddl.String()
}()

// Journal installation is optional for both push and serve schema setup.
func ensureUsageChangeJournalPG(ctx context.Context, pg *sql.DB) {
	var err error
	defer func() {
		if err != nil {
			log.Printf("pg schema: usage change journal setup failed: %v", err)
		}
	}()
	column, err := sessionVersionColumnPG(ctx, pg)
	if err != nil || column != "xmin" {
		return
	}
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	err = installUsageChangeJournalPG(ctx, tx)
	if err != nil {
		return
	}
	err = tx.Commit()
}

func installUsageChangeJournalPG(ctx context.Context, tx *sql.Tx) error {
	if err := lockSyncMetadataRow(ctx, tx, "usage_change_journal_lock"); err != nil {
		return err
	}
	// Any recorded version, including a newer binary's, stays untouched.
	var version string
	err := tx.QueryRowContext(ctx, `SELECT value FROM sync_metadata WHERE key = 'usage_change_journal_version'`).Scan(&version)
	if !errors.Is(err, sql.ErrNoRows) {
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
	var ddl strings.Builder
	ddl.WriteString(`CREATE TABLE IF NOT EXISTS usage_session_changes(session_id TEXT PRIMARY KEY, revision BIGINT NOT NULL);
 CREATE INDEX IF NOT EXISTS idx_usage_session_changes_revision ON usage_session_changes(revision);
 INSERT INTO sync_metadata(key, value) VALUES('usage_change_revision', (extract(epoch FROM clock_timestamp()) * 1000000)::bigint::text) ON CONFLICT(key) DO NOTHING;
`)
	fmt.Fprintf(&ddl, `CREATE OR REPLACE FUNCTION stamp_usage_change() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
 SET search_path = pg_catalog, %s, pg_temp AS $journal$%s$journal$;
 REVOKE ALL ON FUNCTION stamp_usage_change() FROM PUBLIC;
`, quoted, stampUsageChangeFunctionBodyPG)
	ddl.WriteString(`CREATE CONSTRAINT TRIGGER trg_uc_stamp AFTER INSERT OR UPDATE ON usage_session_changes
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.revision = 0) EXECUTE FUNCTION stamp_usage_change();
`)
	for _, table := range usageChangeTablesPG {
		sessionKey, filter := "''", "true"
		columns := "*"
		if table.table == "sessions" {
			// Audit stamps don't change report inputs. Compare every other
			// column, including columns added after journal installation.
			columns = "id, to_jsonb(row_data) - 'automation_audit_hash'"
		}
		if table.sessionColumn != "" {
			sessionKey, err = quoteIdentifier(table.sessionColumn)
			if err != nil {
				return err
			}
		}
		if table.table == "model_pricing" || table.table == "model_pricing_bands" {
			filter = `model_pattern NOT LIKE '\_%'`
		}
		function := "mark_usage_" + table.table
		fmt.Fprintf(&ddl, `CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
 SET search_path = pg_catalog, %s, pg_temp AS $journal$%s$journal$;
 REVOKE ALL ON FUNCTION %s() FROM PUBLIC;
`, function, quoted, fmt.Sprintf(markUsageChangeFunctionBodyPG, sessionKey, filter, columns), function)
		for _, trigger := range []struct{ event, referencing string }{
			{"INSERT", "NEW TABLE AS new_rows"},
			{"UPDATE", "OLD TABLE AS old_rows NEW TABLE AS new_rows"},
			{"DELETE", "OLD TABLE AS old_rows"},
		} {
			fmt.Fprintf(&ddl, "CREATE TRIGGER trg_uc_%s_%s AFTER %s ON %s REFERENCING %s FOR EACH STATEMENT EXECUTE FUNCTION %s();\n",
				table.table, strings.ToLower(trigger.event), trigger.event, table.table, trigger.referencing, function)
		}
	}
	ddl.WriteString(`DO $grants$ DECLARE reader text; BEGIN
 FOR reader IN
     SELECT DISTINCT CASE WHEN acl.grantee = 0 THEN 'PUBLIC' ELSE quote_ident(pg_get_userbyid(acl.grantee)) END
     FROM pg_class c, LATERAL aclexplode(coalesce(c.relacl, acldefault('r', c.relowner))) acl
     WHERE c.oid = 'sessions'::regclass AND acl.privilege_type = 'SELECT'
 LOOP
     EXECUTE 'GRANT SELECT ON usage_session_changes TO ' || reader;
 END LOOP;
 END $grants$;`)
	if _, err = tx.ExecContext(ctx, ddl.String()); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_metadata(key,value) VALUES('usage_change_journal_version',$1)`, strconv.Itoa(usageChangeJournalVersionPG))
	return err
}

// UsageChangeRevision returns the current revision, or 0 when the journal is
// unavailable. See LoadUsageChanges.
func (s *Store) UsageChangeRevision(ctx context.Context) (int64, error) {
	revision, _, err := s.LoadUsageChanges(ctx, math.MaxInt64)
	return revision, err
}

// LoadUsageChanges returns the current revision and sessions changed after
// afterRevision from one snapshot, including hard-deleted sessions. Pricing
// and project identity changes only advance the revision. A write committed
// after this snapshot appears in the next call when the caller uses the
// returned revision. Revision 0 means the journal is absent, unreadable, or
// in another version. A rebuilt schema seeds a newer clock value, so the
// revision cannot identify a reset. Readers must start fresh after a reset.
func (s *Store) LoadUsageChanges(ctx context.Context, afterRevision int64) (int64, []string, error) {
	revision, ids, err := s.loadUsageChanges(ctx, afterRevision)
	if isInsufficientPrivilege(err) || isUndefinedTable(err) {
		return 0, nil, nil
	}
	return revision, ids, err
}

func (s *Store) loadUsageChanges(ctx context.Context, afterRevision int64) (int64, []string, error) {
	rows, err := s.pg.QueryContext(ctx, `SELECT r.revision, c.session_id
 FROM (SELECT coalesce((
  SELECT value::bigint FROM sync_metadata WHERE key = 'usage_change_revision'
  AND EXISTS (SELECT 1 FROM sync_metadata WHERE key = 'usage_change_journal_version' AND value = $1)
 ), 0) AS revision) r
 LEFT JOIN usage_session_changes c ON r.revision > 0 AND c.revision > $2 AND c.revision <= r.revision AND c.session_id <> ''
 ORDER BY c.revision, c.session_id`, strconv.Itoa(usageChangeJournalVersionPG), afterRevision)
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
