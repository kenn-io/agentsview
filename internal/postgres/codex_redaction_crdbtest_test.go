//go:build crdbtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests require a CockroachDB instance (make test-cockroach). They
// cover the CockroachDB port of the Codex encrypted-payload migration:
// trigger-based write guards without PostgreSQL's advisory locks, LOCK
// TABLE, and temp tables, plus the scan-mode read gating fallback.

const crdbCodexFernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="

func testCRDBURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_CRDB_URL")
	if url == "" {
		t.Skip("TEST_CRDB_URL not set; skipping CockroachDB tests")
	}
	return url
}

func openCRDBSchema(t *testing.T, schema string) *sql.DB {
	t.Helper()
	crdbURL := testCRDBURL(t)
	pg, err := Open(crdbURL, schema, true)
	require.NoError(t, err, "connect to CockroachDB")
	// Cleanups run LIFO: register the schema drop first so the pool closes
	// before the DROP SCHEMA runs against CockroachDB.
	t.Cleanup(func() {
		cleanup, cleanupErr := sql.Open("pgx", crdbURL)
		require.NoError(t, cleanupErr, "connect for schema cleanup")
		defer cleanup.Close()
		_, cleanupErr = cleanup.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		require.NoError(t, cleanupErr, "drop test schema")
	})
	t.Cleanup(func() { pg.Close() })
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err, "reset test schema")
	return pg
}

func insertCRDBUncertifiedSession(
	t *testing.T, ctx context.Context, pg *sql.DB, id string,
) {
	t.Helper()
	require.NoError(t, withCodexRepairTxPG(ctx, pg, true, func(tx *sql.Tx) error {
		if err := markCodexPayloadRepairPG(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version, first_message
) VALUES ($1, 'test-machine', 'project', 'codex', 'subagent', 64, $2)`,
			id, crdbCodexFernet)
		return err
	}), "seed intentionally uncertified session %s", id)
}

func TestCRDBDetection(t *testing.T) {
	pg := openCRDBSchema(t, "agentsview_crdb_detect_test")
	ctx := context.Background()
	crdb, err := serverIsCockroachDBPG(ctx, pg)
	require.NoError(t, err, "detect server flavor")
	assert.True(t, crdb, "TEST_CRDB_URL must point at CockroachDB")
}

func TestCRDBEnsureSchemaInstallsCodexPayloadGuards(t *testing.T) {
	pg := openCRDBSchema(t, "agentsview_crdb_codex_guards_test")
	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, "agentsview_crdb_codex_guards_test"),
		"EnsureSchema on CockroachDB")

	installed, err := codexPayloadWriteGuardsInstalledPG(ctx, pg)
	require.NoError(t, err, "probe installed guards")
	assert.True(t, installed, "guards must install on trigger-capable CockroachDB")
	mode, err := codexPayloadGuardModePG(ctx, pg)
	require.NoError(t, err, "read guard mode")
	assert.Equal(t, codexPayloadGuardModeTriggers, mode)

	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version, first_message
) VALUES ('legacy', 'test-machine', 'project', 'codex', 'subagent', 64, 'hello')`)
	require.NoError(t, err, "insert ciphertext-free legacy session")
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (session_id, ordinal, role, content, content_length)
VALUES ('legacy', 0, 'user', $1, $2)`, crdbCodexFernet, len(crdbCodexFernet))
	require.Error(t, err, "legacy ciphertext message must be rejected")
	assert.Contains(t, err.Error(), "23514")
	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version, first_message
) VALUES ('legacy-preview', 'test-machine', 'project', 'codex', 'subagent', 64, $1)`,
		crdbCodexFernet)
	require.Error(t, err, "legacy ciphertext preview must be rejected")
	assert.Contains(t, err.Error(), "23514")
	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version, first_message
) VALUES ('legacy-unlinked-preview', 'test-machine', 'project', 'codex', '', 64, $1)`,
		crdbCodexFernet)
	require.Error(t, err, "unlinked legacy ciphertext preview must be rejected")
	assert.Contains(t, err.Error(), "23514")
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('legacy', 1, 'assistant', $1, $2, TRUE)`,
		crdbCodexFernet, len(crdbCodexFernet))
	require.Error(t, err,
		"legacy tool-use ciphertext without a collab row must be rejected")
	assert.Contains(t, err.Error(), "23514")
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('legacy', 2, 'assistant', $1, $2, FALSE)`,
		crdbCodexFernet, len(crdbCodexFernet))
	require.NoError(t, err, "insert plain assistant token before flag transition")
	_, err = pg.ExecContext(ctx, `
UPDATE messages SET has_tool_use = TRUE
 WHERE session_id = 'legacy' AND ordinal = 2`)
	require.Error(t, err, "changing has_tool_use must re-run the message guard")
	assert.Contains(t, err.Error(), "23514")
	formatted := "[Task: spawn_agent]\n" + crdbCodexFernet
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('legacy', 3, 'assistant', $1, $2, FALSE)`,
		formatted, len(formatted))
	require.Error(t, err,
		"a formatted collaboration block must not need either metadata signal")
	assert.Contains(t, err.Error(), "23514")
	bashFormatted := "[Bash: decrypt]\n$ decrypt " + crdbCodexFernet
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('legacy', 4, 'assistant', $1, $2, FALSE)`,
		bashFormatted, len(bashFormatted))
	require.NoError(t, err,
		"a formatted non-collaboration block must remain permitted")
	_, err = pg.ExecContext(ctx, `
DELETE FROM messages WHERE session_id = 'legacy' AND ordinal = 2`)
	require.NoError(t, err, "remove the preserved plain assistant fixture")

	// CockroachDB has no pgvector, so the vector schema (and with it the
	// vector write guard) is never created there; the guard count check
	// above must already account for that.
	hasVectorDocuments, err := codexPGHasTable(ctx, pg, "vector_documents")
	require.NoError(t, err, "probe vector_documents")
	assert.False(t, hasVectorDocuments,
		"CockroachDB schemas must not gain vector storage")

	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.Error(t, err,
		"a clean session left by a legacy writer must still require certification")
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired)
	require.NoError(t, ensureCodexEncryptedPayloadCompatibilityPG(ctx, pg),
		"certify the clean legacy session")
	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"read gating must pass after certifying the guarded CockroachDB schema")
	require.NoError(t, CheckCodexEncryptedPayloadPersistentReadCompat(ctx, pg),
		"persistent readers may serve a certified guarded CockroachDB schema")
}

func TestCRDBCurationUpdatesUncertifiedSession(t *testing.T) {
	const schema = "agentsview_crdb_codex_curation_test"
	pg := openCRDBSchema(t, schema)
	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, schema),
		"EnsureSchema on CockroachDB")

	insertCRDBUncertifiedSession(t, ctx, pg, "legacy-curation")

	store := &Store{pg: pg}
	require.NoError(t, store.SoftDeleteSession("legacy-curation"),
		"trash without rewriting guarded payload columns")
	var deletedAt sql.NullTime
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT deleted_at FROM sessions WHERE id = 'legacy-curation'`,
	).Scan(&deletedAt), "read trashed state")
	assert.True(t, deletedAt.Valid, "the legacy session must be trashed")

	restored, err := store.RestoreSession("legacy-curation")
	require.NoError(t, err,
		"restore without rewriting guarded payload columns")
	assert.EqualValues(t, 1, restored)
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT deleted_at FROM sessions WHERE id = 'legacy-curation'`,
	).Scan(&deletedAt), "read restored state")
	assert.False(t, deletedAt.Valid, "the legacy session must be restored")

	_, err = pg.ExecContext(ctx, `
UPDATE sessions SET relationship_type = ''
 WHERE id = 'legacy-curation'`)
	require.Error(t, err,
		"changing guarded relationship evidence must still be rejected")
	assert.Contains(t, err.Error(), "23514")

	_, err = pg.ExecContext(ctx, `
UPDATE sessions SET first_message = first_message || ' changed'
 WHERE id = 'legacy-curation'`)
	require.Error(t, err,
		"changing guarded payload evidence must still be rejected")
	assert.Contains(t, err.Error(), "23514")
}

func TestCRDBUpgradesSessionGuardV6(t *testing.T) {
	const schema = "agentsview_crdb_codex_guard_v6_upgrade_test"
	pg := openCRDBSchema(t, schema)
	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, schema),
		"EnsureSchema on CockroachDB")

	_, err := pg.ExecContext(ctx, fmt.Sprintf(`
DROP TRIGGER %s ON sessions;
DROP TRIGGER IF EXISTS %s ON sessions;
CREATE OR REPLACE FUNCTION agentsview_guard_codex_payload_session()
RETURNS trigger LANGUAGE plpgsql AS $function$
BEGIN
    IF TG_OP = 'UPDATE'
       AND (NEW).agent IS NOT DISTINCT FROM (OLD).agent
       AND (NEW).data_version IS NOT DISTINCT FROM (OLD).data_version
       AND (NEW).first_message IS NOT DISTINCT FROM (OLD).first_message THEN
        RETURN NEW;
    END IF;
    IF (NEW).agent = 'codex'
       AND (NEW).data_version < %d
       AND current_setting('%s', true) IS DISTINCT FROM '%d'
       AND COALESCE((NEW).first_message, '') LIKE '%%gAAAAA%%' THEN
        RAISE EXCEPTION 'legacy Codex preview contains an encrypted payload'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$function$;
CREATE TRIGGER %s
BEFORE INSERT OR UPDATE ON sessions
FOR EACH ROW EXECUTE FUNCTION agentsview_guard_codex_payload_session()`,
		codexSessionWriteGuardTrigger, previousCodexSessionWriteGuardV6,
		codexEncryptedPayloadDataVersion, codexPayloadRepairSetting,
		codexEncryptedPayloadDataVersion, previousCodexSessionWriteGuardV6))
	require.NoError(t, err, "install the previous v6 session guard")

	insertCRDBUncertifiedSession(t, ctx, pg, "legacy-v6")
	_, err = pg.ExecContext(ctx, `
UPDATE sessions SET relationship_type = '' WHERE id = 'legacy-v6'`)
	require.NoError(t, err,
		"the v6 fixture must reproduce the relationship-only bypass")

	require.NoError(t, ensureCodexEncryptedPayloadCompatibilityPG(ctx, pg),
		"upgrade the v6 guard")
	installed, err := codexPayloadWriteGuardsInstalledPG(ctx, pg)
	require.NoError(t, err, "probe upgraded guards")
	assert.True(t, installed, "the current guard generation must be installed")
	var previousCount int
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT COUNT(*) FROM pg_trigger WHERE tgname = $1`,
		previousCodexSessionWriteGuardV6).Scan(&previousCount),
		"count previous session guards")
	assert.Zero(t, previousCount, "the v6 session guard must be removed")

	insertCRDBUncertifiedSession(t, ctx, pg, "legacy-v7")
	_, err = pg.ExecContext(ctx, `
UPDATE sessions SET relationship_type = '' WHERE id = 'legacy-v7'`)
	require.Error(t, err,
		"the upgraded guard must reject relationship-only mutations")
	assert.Contains(t, err.Error(), "23514")
}

func TestCRDBEnsureSchemaRepairsLegacyCiphertext(t *testing.T) {
	const schema = "agentsview_crdb_codex_repair_test"
	pg := openCRDBSchema(t, schema)
	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, schema),
		"EnsureSchema on CockroachDB")

	// Simulate a database written by builds that predate the guards: drop
	// the triggers and markers, then land legacy ciphertext rows.
	_, err := pg.ExecContext(ctx, fmt.Sprintf(`
DROP TRIGGER %s ON sessions;
DROP TRIGGER %s ON messages;
DROP TRIGGER %s ON tool_calls`,
		codexSessionWriteGuardTrigger, codexMessageWriteGuardTrigger,
		codexToolWriteGuardTrigger))
	require.NoError(t, err, "simulate a legacy schema without guards")
	_, err = pg.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key IN ($1, $2)`,
		codexVectorRepairCompletedMetadata, codexPayloadGuardModeMetadata)
	require.NoError(t, err, "clear migration markers")

	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version, first_message
) VALUES ('legacy', 'test-machine', 'project', 'codex', 'subagent', 64, $1)`,
		crdbCodexFernet)
	require.NoError(t, err, "insert legacy ciphertext preview")
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (session_id, ordinal, role, content, content_length)
VALUES ('legacy', 0, 'user', $1, $2)`, crdbCodexFernet, len(crdbCodexFernet))
	require.NoError(t, err, "insert legacy ciphertext message")
	headerContent := "[Task: " + crdbCodexFernet + "]\nRun the task"
	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version
) VALUES ('encrypted-header', 'test-machine', 'project', 'codex', '', 67)`)
	require.NoError(t, err, "insert legacy header session")
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('encrypted-header', 0, 'assistant', $1, $2, TRUE)`,
		headerContent, len(headerContent))
	require.NoError(t, err, "insert legacy encrypted collaboration header")
	_, err = pg.ExecContext(ctx, `
INSERT INTO tool_calls (
    session_id, tool_name, category, message_ordinal, input_json
) VALUES (
    'encrypted-header', 'spawn_agent', 'Task', 0,
    '{"task_name":"[encrypted]","message":"Run the task"}'
)`)
	require.NoError(t, err, "insert current redacted tool input")

	require.NoError(t, ensureCodexEncryptedPayloadCompatibilityPG(ctx, pg),
		"rerun the migration against legacy rows")

	var preview, content string
	var dataVersion int
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT s.first_message, s.data_version, m.content
  FROM sessions s JOIN messages m ON m.session_id = s.id
 WHERE s.id = 'legacy' AND m.ordinal = 0`,
	).Scan(&preview, &dataVersion, &content))
	assert.NotContains(t, preview, "gAAAAA", "preview ciphertext must be redacted")
	assert.Equal(t, "[encrypted]", content, "message ciphertext must be redacted")
	assert.Equal(t, codexEncryptedPayloadDataVersion, dataVersion,
		"repaired sessions must be promoted")

	var gotHeader string
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT m.content, s.data_version
  FROM sessions s JOIN messages m ON m.session_id = s.id
 WHERE s.id = 'encrypted-header' AND m.ordinal = 0`,
	).Scan(&gotHeader, &dataVersion))
	assert.Equal(t, "[Task: [encrypted]]\nRun the task", gotHeader,
		"legacy collaboration headers must be repaired")
	assert.Equal(t, codexEncryptedPayloadDataVersion, dataVersion,
		"the repaired header session must be promoted")

	installed, err := codexPayloadWriteGuardsInstalledPG(ctx, pg)
	require.NoError(t, err, "probe reinstalled guards")
	assert.True(t, installed, "the migration must reinstall the guards")
	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"read gating must pass after the repair")
	require.NoError(t, CheckCodexEncryptedPayloadPersistentReadCompat(ctx, pg),
		"persistent readers may serve after the guards are reinstalled")
}

func TestCRDBScanModeReadGating(t *testing.T) {
	const schema = "agentsview_crdb_codex_scanmode_test"
	pg := openCRDBSchema(t, schema)
	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, schema),
		"EnsureSchema on CockroachDB")

	// Simulate a server without trigger support (CockroachDB before v24.3):
	// the migration records scan mode and installs no triggers.
	_, err := pg.ExecContext(ctx, fmt.Sprintf(`
DROP TRIGGER %s ON sessions;
DROP TRIGGER %s ON messages;
DROP TRIGGER %s ON tool_calls`,
		codexSessionWriteGuardTrigger, codexMessageWriteGuardTrigger,
		codexToolWriteGuardTrigger))
	require.NoError(t, err, "drop guards to simulate scan mode")

	// Guard mode "triggers" with missing guards must fail closed: something
	// removed guards the migration recorded as installed.
	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.Error(t, err,
		"recorded triggers with missing guards must fail closed")
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired)

	require.NoError(t, setCodexPayloadGuardModePG(
		ctx, pg, codexPayloadGuardModeScan), "record scan mode")
	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"repair verification may admit clean scan mode")
	err = CheckCodexEncryptedPayloadBoundedReadCompat(ctx, pg)
	require.Error(t, err,
		"bounded readers must reject a scan outside their query snapshot")
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired)
	assert.Contains(t, err.Error(), "bounded shared-storage reads")
	err = CheckCodexEncryptedPayloadPersistentReadCompat(ctx, pg)
	require.Error(t, err,
		"persistent readers must reject triggerless scan mode even while clean")
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired)
	assert.Contains(t, err.Error(), "persistent shared-storage serving")

	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version, first_message
) VALUES ('late-legacy', 'test-machine', 'project', 'codex', 'subagent', 64, $1)`,
		crdbCodexFernet)
	require.NoError(t, err, "land a late legacy write in scan mode")
	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.Error(t, err, "scan mode must fail closed on legacy ciphertext")
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired)

	// A missing guard-mode marker on CockroachDB still falls back to the
	// scans instead of demanding trigger guards the server may not support.
	_, err = pg.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = $1`, codexPayloadGuardModeMetadata)
	require.NoError(t, err, "clear guard mode marker")
	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.Error(t, err, "markerless scan gating must still fail closed")
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired)
	assert.False(t, strings.Contains(err.Error(), "write guards"),
		"CockroachDB gating must ask for the repair, not for trigger guards")
}
