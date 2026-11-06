package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
)

// Statement triggers mark each changed session pending with revision 0 in one
// set-based write. The empty session ID marks pricing and identity changes.
const markUsageChangeFunctionBodyPG = `
DECLARE
    session_key text := '''''';
    filter text := 'true';
    changed text := 'SELECT * FROM new_rows';
BEGIN
    IF TG_ARGV[0] <> '' THEN
        session_key := quote_ident(TG_ARGV[0]);
    END IF;
    IF TG_TABLE_NAME IN ('model_pricing', 'model_pricing_bands') THEN
        filter := 'model_pattern NOT LIKE ''\_%''';
    END IF;
    IF TG_OP = 'DELETE' THEN
        changed := 'SELECT * FROM old_rows';
    ELSIF TG_OP = 'UPDATE' THEN
        changed := '(SELECT * FROM old_rows EXCEPT SELECT * FROM new_rows)
 UNION ALL (SELECT * FROM new_rows EXCEPT SELECT * FROM old_rows)';
    END IF;
    EXECUTE format('INSERT INTO usage_session_changes(session_id, revision)
 SELECT DISTINCT %s, 0 FROM (%s) changed WHERE %s
 ON CONFLICT(session_id) DO UPDATE SET revision = 0 WHERE usage_session_changes.revision <> 0',
        session_key, changed, filter);
    RETURN NULL;
END;
`

// The deferred stamp takes the counter lock at commit and holds it through
// commit, so revisions follow commit order. Committed rows are never pending,
// so later queued calls in the same transaction return without a revision.
const stampUsageChangeFunctionBodyPG = `
DECLARE
    stamped bigint;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM usage_session_changes WHERE revision = 0) THEN
        RETURN NULL;
    END IF;
    INSERT INTO sync_metadata(key, value)
    VALUES('usage_change_revision', (extract(epoch FROM clock_timestamp()) * 1000000)::bigint::text)
    ON CONFLICT(key) DO UPDATE SET value = (sync_metadata.value::bigint + 1)::text
    RETURNING value::bigint INTO stamped;
    UPDATE usage_session_changes SET revision = stamped WHERE revision = 0;
    RETURN NULL;
END;
`

const usageChangeJournalVersionPG = 1

// Messages and plan usage reach the journal through the sessions.updated_at
// bump every released publisher writes with them.
var usageChangeTablesPG = []struct{ table, sessionColumn string }{
	{"sessions", "id"}, {"usage_events", "session_id"},
	{"model_pricing", ""}, {"model_pricing_bands", ""}, {"genai_pricing", ""}, {"cursor_usage_events", ""},
	{"source_project_identity_observations", ""}, {"source_project_identity_observation_scopes", ""},
	{"source_session_project_identity_snapshots", ""}, {"source_session_project_identity_snapshot_scopes", ""},
	{"source_worktree_project_mappings", ""}, {"source_worktree_project_mapping_scopes", ""},
}

// usageChangeJournalInstalledPG reports whether any journal version is
// recorded, so a newer binary's journal stays untouched.
func usageChangeJournalInstalledPG(ctx context.Context, q hostedQuerier) (bool, error) {
	var version string
	err := q.QueryRowContext(ctx, `SELECT value FROM sync_metadata WHERE key = 'usage_change_journal_version'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func ensureUsageChangeJournalPG(ctx context.Context, pg *sql.DB) error {
	column, err := sessionVersionColumnPG(ctx, pg)
	if err != nil || column != "xmin" {
		return err
	}
	installed, err := usageChangeJournalInstalledPG(ctx, pg)
	if err != nil || installed {
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
	installed, err := usageChangeJournalInstalledPG(ctx, tx)
	if err != nil || installed {
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
	for _, function := range []struct{ name, body string }{
		{"mark_usage_change", markUsageChangeFunctionBodyPG},
		{"stamp_usage_change", stampUsageChangeFunctionBodyPG},
	} {
		fmt.Fprintf(&ddl, `CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER
 SET search_path = pg_catalog, %s, pg_temp AS $journal$%s$journal$;
 REVOKE ALL ON FUNCTION %s() FROM PUBLIC;
`, function.name, quoted, function.body, function.name)
	}
	ddl.WriteString(`CREATE CONSTRAINT TRIGGER trg_uc_stamp AFTER INSERT OR UPDATE ON usage_session_changes
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.revision = 0) EXECUTE FUNCTION stamp_usage_change();
`)
	for _, table := range usageChangeTablesPG {
		for _, trigger := range []struct{ event, referencing string }{
			{"INSERT", "NEW TABLE AS new_rows"},
			{"UPDATE", "OLD TABLE AS old_rows NEW TABLE AS new_rows"},
			{"DELETE", "OLD TABLE AS old_rows"},
		} {
			fmt.Fprintf(&ddl, "CREATE TRIGGER trg_uc_%s_%s AFTER %s ON %s REFERENCING %s FOR EACH STATEMENT EXECUTE FUNCTION mark_usage_change('%s');\n",
				table.table, strings.ToLower(trigger.event), trigger.event, table.table, trigger.referencing, table.sessionColumn)
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
	_, err = tx.ExecContext(ctx, `INSERT INTO sync_metadata(key,value) VALUES('usage_change_journal_version',$1)
 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(usageChangeJournalVersionPG))
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
// in another version; a revision below afterRevision means a rebuilt archive.
// Callers rebuild in both cases.
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
 ORDER BY c.revision, c.session_id`, fmt.Sprint(usageChangeJournalVersionPG), afterRevision)
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
