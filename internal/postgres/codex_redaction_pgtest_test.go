//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const codexRedactionTestSchema = "agentsview_codex_redaction_test"

func TestRepairCodexVectorsRechecksAfterConcurrentMutation(t *testing.T) {
	const schema = "agentsview_codex_redaction_lock_test"
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "connect to PG")
	defer pg.Close()
	t.Cleanup(func() {
		cleanup, cleanupErr := sql.Open("pgx", pgURL)
		require.NoError(t, cleanupErr, "connect for schema cleanup")
		defer cleanup.Close()
		_, cleanupErr = cleanup.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		require.NoError(t, cleanupErr, "drop test schema")
	})

	ctx := context.Background()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err, "reset test schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "create current schema")
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO sessions (id, machine, project, agent, data_version)
VALUES ('codex-clean', 'test-machine', 'project', 'codex', %d)`,
		codexEncryptedPayloadDataVersion))
	require.NoError(t, err, "insert clean Codex session")
	seedVectorTx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "begin current vector seed")
	require.NoError(t, lockCodexVectorMutationSharedPG(ctx, seedVectorTx, false),
		"mark current vector seed")
	_, err = seedVectorTx.ExecContext(ctx, `
INSERT INTO vector_documents (
    doc_key, session_id, ordinal, ordinal_end, content, content_hash
) VALUES ('clean-doc', 'codex-clean', 0, 0, '[encrypted]', 'fresh-hash')`)
	require.NoError(t, err, "insert fresh Codex vector")
	require.NoError(t, seedVectorTx.Commit(), "commit current vector seed")
	_, err = pg.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = $1`,
		codexVectorRepairCompletedMetadata,
	)
	require.NoError(t, err, "clear repair marker")

	mutationTx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "begin concurrent vector mutation")
	defer func() { _ = mutationTx.Rollback() }()
	require.NoError(t, lockCodexVectorRepairExclusivePG(ctx, mutationTx, false),
		"hold vector repair lock")

	done := make(chan error, 1)
	go func() {
		done <- repairCodexEncryptedPayloadsPG(ctx, pg, false)
	}()
	select {
	case repairErr := <-done:
		require.Failf(t, "repair completed during vector mutation",
			"repair returned before the concurrent mutation committed: %v", repairErr)
	case <-time.After(250 * time.Millisecond):
	}

	require.NoError(t, markCodexVectorRepairCompletePG(ctx, mutationTx),
		"record completed vector repair in concurrent transaction")
	require.NoError(t, mutationTx.Commit(), "commit concurrent vector mutation")
	select {
	case repairErr := <-done:
		require.NoError(t, repairErr, "repair after concurrent mutation")
	case <-time.After(5 * time.Second):
		require.Fail(t, "repair did not resume after concurrent mutation")
	}

	var count int
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT COUNT(*) FROM vector_documents WHERE doc_key = 'clean-doc'`).Scan(&count))
	assert.Equal(t, 1, count,
		"repair must recheck the marker and preserve the concurrently validated vector")

	_, err = pg.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = $1`,
		codexVectorRepairCompletedMetadata,
	)
	require.NoError(t, err, "clear repair marker for marker-only race")
	_, err = pg.ExecContext(ctx, `DELETE FROM vector_documents`)
	require.NoError(t, err, "clear vectors for marker-only race")

	vectorTx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "begin concurrent vector push")
	defer func() { _ = vectorTx.Rollback() }()
	require.NoError(t, lockCodexVectorMutationSharedPG(ctx, vectorTx, false),
		"hold shared vector mutation lock")
	done = make(chan error, 1)
	go func() {
		done <- repairCodexEncryptedPayloadsPG(ctx, pg, false)
	}()
	select {
	case repairErr := <-done:
		require.Failf(t, "marker repair completed during vector push",
			"repair returned before the concurrent vector push committed: %v", repairErr)
	case <-time.After(250 * time.Millisecond):
	}

	_, err = vectorTx.ExecContext(ctx, `
INSERT INTO vector_documents (
    doc_key, session_id, ordinal, ordinal_end, content, content_hash
) VALUES ('racing-doc', 'codex-clean', 0, 0, '[encrypted]', 'racing-hash')`)
	require.NoError(t, err, "insert concurrently pushed Codex vector")
	require.NoError(t, vectorTx.Commit(), "commit concurrent vector push")
	select {
	case repairErr := <-done:
		require.NoError(t, repairErr, "marker repair after concurrent vector push")
	case <-time.After(5 * time.Second):
		require.Fail(t, "marker repair did not resume after concurrent vector push")
	}
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT COUNT(*) FROM vector_documents WHERE doc_key = 'racing-doc'`).Scan(&count))
	assert.Zero(t, count,
		"the first repair must sweep a vector committed before its exclusive lock")
}

func cleanCodexRedactionTestPG(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err, "connect for schema cleanup")
	defer pg.Close()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + codexRedactionTestSchema + " CASCADE")
	require.NoError(t, err, "drop Codex redaction test schema")
}

func TestEnsureCodexCompatibilityWithoutVectorDocuments(t *testing.T) {
	const schema = "agentsview_codex_no_vectors_test"
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "connect to PG")
	defer pg.Close()
	t.Cleanup(func() {
		cleanup, cleanupErr := sql.Open("pgx", pgURL)
		require.NoError(t, cleanupErr, "connect for schema cleanup")
		defer cleanup.Close()
		_, cleanupErr = cleanup.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		require.NoError(t, cleanupErr, "drop test schema")
	})

	ctx := context.Background()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err, "reset test schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "create current schema")
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
DROP TRIGGER %s ON sessions;
DROP TRIGGER %s ON messages;
DROP TRIGGER %s ON tool_calls;
DROP TABLE vector_documents CASCADE`,
		codexSessionWriteGuardTrigger, codexMessageWriteGuardTrigger,
		codexToolWriteGuardTrigger))
	require.NoError(t, err, "simulate a schema without vector support or guards")

	require.NoError(t, ensureCodexEncryptedPayloadCompatibilityPG(ctx, pg),
		"install core payload guards without optional vector tables")
	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"three core guards are sufficient when vector documents are unavailable")
	require.NoError(t, CheckCodexEncryptedPayloadPersistentReadCompat(ctx, pg),
		"persistent reads are safe when all applicable guards are installed")
	var vectorDocumentsPresent bool
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT to_regclass(current_schema() || '.vector_documents') IS NOT NULL`,
	).Scan(&vectorDocumentsPresent))
	assert.False(t, vectorDocumentsPresent,
		"payload compatibility must not create optional vector storage")
}

func TestEnsureCodexCompatibilityRepairsConcurrentLegacyPush(t *testing.T) {
	const schema = "agentsview_codex_guard_race_test"
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "connect to PG")
	defer pg.Close()
	t.Cleanup(func() {
		cleanup, cleanupErr := sql.Open("pgx", pgURL)
		require.NoError(t, cleanupErr, "connect for schema cleanup")
		defer cleanup.Close()
		_, cleanupErr = cleanup.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		require.NoError(t, cleanupErr, "drop test schema")
	})

	ctx := context.Background()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err, "reset test schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "create current schema")
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
DROP TRIGGER %s ON sessions;
DROP TRIGGER %s ON messages;
DROP TRIGGER %s ON tool_calls;
DROP TRIGGER %s ON vector_documents`,
		codexSessionWriteGuardTrigger, codexMessageWriteGuardTrigger,
		codexToolWriteGuardTrigger, codexVectorWriteGuardTrigger))
	require.NoError(t, err, "simulate a legacy schema without guards")

	legacyTx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "begin concurrent legacy push")
	defer func() { _ = legacyTx.Rollback() }()
	_, err = legacyTx.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version
) VALUES ('concurrent-legacy', 'test-machine', 'project', 'codex', 'subagent', 64)`)
	require.NoError(t, err, "insert concurrent legacy session")
	_, err = legacyTx.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length
) VALUES ('concurrent-legacy', 0, 'user', $1, $2)`, fernet, len(fernet))
	require.NoError(t, err, "insert concurrent legacy ciphertext")

	done := make(chan error, 1)
	go func() {
		done <- ensureCodexEncryptedPayloadCompatibilityPG(ctx, pg)
	}()
	select {
	case ensureErr := <-done:
		require.Failf(t, "guard migration bypassed concurrent writer",
			"migration returned before the legacy push committed: %v", ensureErr)
	case <-time.After(250 * time.Millisecond):
	}
	require.NoError(t, legacyTx.Commit(), "commit concurrent legacy push")
	select {
	case ensureErr := <-done:
		require.NoError(t, ensureErr, "install guards after concurrent legacy push")
	case <-time.After(5 * time.Second):
		require.Fail(t, "guard migration did not resume after legacy push")
	}

	var content string
	var dataVersion int
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT m.content, s.data_version
  FROM messages m
  JOIN sessions s ON s.id = m.session_id
 WHERE m.session_id = 'concurrent-legacy' AND m.ordinal = 0`,
	).Scan(&content, &dataVersion))
	assert.Equal(t, "[encrypted]", content)
	assert.Equal(t, codexEncryptedPayloadDataVersion, dataVersion)
	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"the locked migration must leave no compatibility gap")
}

func TestEnsureSchemaRepairsLegacyCodexEncryptedHeader(t *testing.T) {
	const (
		schema            = "agentsview_codex_legacy_header_repair_test"
		fernet            = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
		legacyDataVersion = 67
	)
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "connect to PG")
	defer pg.Close()
	t.Cleanup(func() {
		cleanup, cleanupErr := sql.Open("pgx", pgURL)
		require.NoError(t, cleanupErr, "connect for schema cleanup")
		defer cleanup.Close()
		_, cleanupErr = cleanup.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		require.NoError(t, cleanupErr, "drop test schema")
	})

	ctx := context.Background()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err, "reset test schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "create current schema")
	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version
) VALUES ('encrypted-header', 'test-machine', 'project', 'codex', '', $1)`,
		codexEncryptedPayloadDataVersion)
	require.NoError(t, err, "seed current session before reproducing legacy residue")
	content := "[Task: " + fernet + "]\nRun the task"
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, has_tool_use, content_length
) VALUES ('encrypted-header', 0, 'assistant', $1, TRUE, $2)`,
		content, len(content))
	require.NoError(t, err, "seed encrypted collaboration header")
	_, err = pg.ExecContext(ctx, `
INSERT INTO tool_calls (
    session_id, tool_name, category, message_ordinal, input_json
) VALUES (
    'encrypted-header', 'spawn_agent', 'Task', 0,
    '{"task_name":"[encrypted]","message":"Run the task"}'
)`)
	require.NoError(t, err, "seed current redacted tool input")
	_, err = pg.ExecContext(ctx, `
UPDATE sessions SET data_version = $1 WHERE id = 'encrypted-header'`, legacyDataVersion)
	require.NoError(t, err, "reproduce legacy header residue")

	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"legacy header ciphertext must fail closed before repair")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "repair legacy header")
	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"repaired header must pass PostgreSQL compatibility")

	var gotContent string
	var gotLength, gotVersion int
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT m.content, m.content_length, s.data_version
  FROM messages m
  JOIN sessions s ON s.id = m.session_id
 WHERE s.id = 'encrypted-header' AND m.ordinal = 0`,
	).Scan(&gotContent, &gotLength, &gotVersion))
	want := "[Task: [encrypted]]\nRun the task"
	assert.Equal(t, want, gotContent)
	assert.Equal(t, len(want), gotLength)
	assert.Equal(t, codexEncryptedPayloadDataVersion, gotVersion)
}

func TestEnsureSchemaReplacesLegacyCodexGuardGeneration(t *testing.T) {
	const schema = "agentsview_codex_legacy_guard_upgrade_test"
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "connect to PG")
	defer pg.Close()
	t.Cleanup(func() {
		cleanup, cleanupErr := sql.Open("pgx", pgURL)
		require.NoError(t, cleanupErr, "connect for schema cleanup")
		defer cleanup.Close()
		_, cleanupErr = cleanup.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		require.NoError(t, cleanupErr, "drop test schema")
	})

	ctx := context.Background()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err, "reset test schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "create current schema")
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
ALTER TRIGGER %s ON sessions RENAME TO %s;
ALTER TRIGGER %s ON messages RENAME TO %s;
ALTER TRIGGER %s ON tool_calls RENAME TO %s;
ALTER TRIGGER %s ON vector_documents RENAME TO %s`,
		codexSessionWriteGuardTrigger, previousCodexSessionWriteGuardV4,
		codexMessageWriteGuardTrigger, previousCodexMessageWriteGuardV4,
		codexToolWriteGuardTrigger, previousCodexToolWriteGuardV3,
		codexVectorWriteGuardTrigger, previousCodexVectorWriteGuardV3,
	))
	require.NoError(t, err, "restore the persisted legacy guard names")

	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"legacy guard names must not be accepted as current")
	require.NoError(t, EnsureSchema(ctx, pg, schema),
		"replace the legacy guard generation")
	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"the replacement guard generation must pass compatibility")
	installed, err := codexPayloadWriteGuardsInstalledPG(ctx, pg)
	require.NoError(t, err, "probe replacement guards")
	assert.True(t, installed, "all current guards must be installed")
}

func TestEnsureSchemaWithholdsWatermarkFromUncertifiedLegacyCodexPayloads(
	t *testing.T,
) {
	const (
		schema            = "agentsview_codex_legacy_recertification_test"
		fernet            = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
		legacyDataVersion = 67
	)
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "connect to PG")
	defer pg.Close()
	t.Cleanup(func() {
		cleanup, cleanupErr := sql.Open("pgx", pgURL)
		require.NoError(t, cleanupErr, "connect for schema cleanup")
		defer cleanup.Close()
		_, cleanupErr = cleanup.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		require.NoError(t, cleanupErr, "drop test schema")
	})

	ctx := context.Background()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err, "reset test schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "create current schema")
	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version
) VALUES
    ('unlinked-child', 'test-machine', 'project', 'codex', '', $1),
    ('lost-tool-call', 'test-machine', 'project', 'codex', '', $1),
    ('stale-missing-tool-call', 'test-machine', 'project', 'codex', '', $1)`,
		codexEncryptedPayloadDataVersion)
	require.NoError(t, err, "seed current sessions before reproducing legacy residue")
	formatted := "[Task: spawn_agent]\n" + fernet
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, has_tool_use, content_length
) VALUES
    ('unlinked-child', 0, 'user', $1, FALSE, $2),
    ('lost-tool-call', 0, 'assistant', $3, TRUE, $4),
    ('stale-missing-tool-call', 0, 'assistant', $3, FALSE, $4)`,
		fernet, len(fernet), formatted, len(formatted))
	require.NoError(t, err, "seed metadata-independent ciphertext shapes")
	_, err = pg.ExecContext(ctx, `
INSERT INTO tool_calls (
    session_id, tool_name, category, message_ordinal, input_json
) VALUES ('lost-tool-call', 'Bash', 'Bash', 0, '{}')`)
	require.NoError(t, err,
		"leave a non-collab tool row beside the lost collaboration call")
	_, err = pg.ExecContext(ctx, `
UPDATE sessions SET data_version = $1
 WHERE id IN (
    'unlinked-child', 'lost-tool-call', 'stale-missing-tool-call'
)`, legacyDataVersion)
	require.NoError(t, err, "reproduce falsely certified legacy sessions")

	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"legacy rows must fail closed until the stricter recertification runs")
	require.NoError(t, EnsureSchema(ctx, pg, schema),
		"recheck legacy sessions")
	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"uncertified rows must keep PostgreSQL reads gated after the repair pass")

	rows, err := pg.QueryContext(ctx, `
SELECT s.id, s.data_version, m.content
  FROM sessions s
  JOIN messages m ON m.session_id = s.id
 WHERE s.id IN (
    'unlinked-child', 'lost-tool-call', 'stale-missing-tool-call'
 )
 ORDER BY s.id`)
	require.NoError(t, err, "query recertified sessions")
	defer rows.Close()
	type repairedRow struct {
		id      string
		version int
		content string
	}
	var got []repairedRow
	for rows.Next() {
		var row repairedRow
		require.NoError(t, rows.Scan(&row.id, &row.version, &row.content))
		got = append(got, row)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []repairedRow{
		{id: "lost-tool-call", version: legacyDataVersion, content: formatted},
		{id: "stale-missing-tool-call", version: legacyDataVersion, content: formatted},
		{id: "unlinked-child", version: legacyDataVersion, content: fernet},
	}, got)
}

func TestRepairCodexVectorsScopesInvalidationToStaleSessions(t *testing.T) {
	const schema = "agentsview_codex_scoped_invalidation_test"
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "connect to PG")
	defer pg.Close()
	t.Cleanup(func() {
		cleanup, cleanupErr := sql.Open("pgx", pgURL)
		require.NoError(t, cleanupErr, "connect for schema cleanup")
		defer cleanup.Close()
		_, cleanupErr = cleanup.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		require.NoError(t, cleanupErr, "drop test schema")
	})

	ctx := context.Background()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err, "reset test schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "create current schema")

	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version
) VALUES
    ('codex-stale', 'test-machine', 'project', 'codex', 'subagent', $1),
    ('codex-clean', 'test-machine', 'project', 'codex', 'subagent', $1)`,
		codexEncryptedPayloadDataVersion)
	require.NoError(t, err, "insert repaired Codex sessions")
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (session_id, ordinal, role, content, content_length)
VALUES ('codex-stale', 0, 'user', $1, $2),
       ('codex-clean', 0, 'user', 'plain subagent turn', 19)`,
		fernet, len(fernet))
	require.NoError(t, err, "insert subagent user turns")

	seedTx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "begin vector seed")
	require.NoError(t, lockCodexVectorMutationSharedPG(ctx, seedTx, false),
		"mark vector seed")
	_, err = seedTx.ExecContext(ctx, `
INSERT INTO vector_documents (
    doc_key, session_id, ordinal, ordinal_end, content, content_hash
) VALUES ('stale-doc', 'codex-stale', 0, 0, $1, 'stale-hash'),
         ('clean-doc', 'codex-clean', 0, 0, 'plain subagent turn', 'clean-hash')`,
		fernet)
	require.NoError(t, err, "seed one stale and one fresh Codex vector")
	require.NoError(t, seedTx.Commit(), "commit vector seed")

	require.NoError(t, repairCodexEncryptedPayloadsPG(ctx, pg, false),
		"repair with a completed marker and one stale vector session")

	var staleDocs, cleanDocs int
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT COUNT(*) FILTER (WHERE session_id = 'codex-stale'),
       COUNT(*) FILTER (WHERE session_id = 'codex-clean')
  FROM vector_documents`).Scan(&staleDocs, &cleanDocs))
	assert.Zero(t, staleDocs, "stale ciphertext vectors must be invalidated")
	assert.Equal(t, 1, cleanDocs,
		"the scoped invalidation must not delete fresh vectors of other sessions")
}

func TestEnsureSchemaRepairsOldCodexPayloadsAndVectors(t *testing.T) {
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	pgURL := testPGURL(t)
	cleanCodexRedactionTestPG(t, pgURL)
	t.Cleanup(func() { cleanCodexRedactionTestPG(t, pgURL) })

	pg, err := Open(pgURL, codexRedactionTestSchema, true)
	require.NoError(t, err, "connect to PG")
	defer pg.Close()
	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, codexRedactionTestSchema),
		"create current schema")
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
DROP TRIGGER %s ON sessions;
DROP TRIGGER %s ON messages;
DROP TRIGGER %s ON tool_calls;
DROP TRIGGER %s ON vector_documents`,
		codexSessionWriteGuardTrigger, codexMessageWriteGuardTrigger,
		codexToolWriteGuardTrigger, codexVectorWriteGuardTrigger))
	require.NoError(t, err, "simulate the schema of a legacy PG writer")

	insertSession := func(id, agent, relationship, preview string, version int) {
		t.Helper()
		_, err := pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, first_message, data_version
) VALUES ($1, 'test-machine', 'project', $2, $3, $4, $5)`,
			id, agent, relationship, preview, version)
		require.NoError(t, err, "insert session %s", id)
	}
	insertMessage := func(sessionID string, ordinal int, role, content string, tool bool) {
		t.Helper()
		_, err := pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ($1, $2, $3, $4, $5, $6)`,
			sessionID, ordinal, role, content, len(content), tool)
		require.NoError(t, err, "insert message %s/%d", sessionID, ordinal)
	}
	execCurrentVectorMutation := func(label, query string, args ...any) {
		t.Helper()
		tx, err := pg.BeginTx(ctx, nil)
		require.NoError(t, err, "begin %s", label)
		require.NoError(t, lockCodexVectorMutationSharedPG(ctx, tx, false),
			"mark %s as current", label)
		_, err = tx.ExecContext(ctx, query, args...)
		require.NoError(t, err, label)
		require.NoError(t, tx.Commit(), "commit %s", label)
	}

	insertSession("codex-old", "codex", "subagent", fernet, 64)
	toolContent := "[Task: spawn_agent]\n" + fernet
	// The collab row is authoritative even when an older writer left the
	// cached message flag false.
	insertMessage("codex-old", 0, "assistant", toolContent, false)
	insertMessage("codex-old", 1, "user", fernet, false)
	_, err = pg.ExecContext(ctx, `
INSERT INTO tool_calls (
    session_id, tool_name, category, message_ordinal, input_json
) VALUES ($1, 'spawn_agent', 'Task', 0, $2)`,
		"codex-old", `{"task_name":"worker","message":"`+fernet+`"}`)
	require.NoError(t, err, "insert stale collab tool call")
	_, err = pg.ExecContext(ctx, `
UPDATE sessions
   SET quality_signal_version = 2, short_prompt_count = 3,
	   health_score = 17, health_grade = 'D',
       unstructured_start = TRUE, missing_success_criteria_count = 4,
       missing_verification_count = 5, duplicate_prompt_count = 6,
       no_code_context_count = 7, runaway_tool_loop_count = 8,
       secret_leak_count = 1, secrets_rules_version = 'stale-rules',
       transcript_revision = '7'
 WHERE id = 'codex-old'`)
	require.NoError(t, err, "seed stale PG derived data")
	_, err = pg.ExecContext(ctx, `
INSERT INTO secret_findings (
    session_id, rule_name, confidence, location_kind,
    message_ordinal, match_start, match_end, match_index,
    redacted_match, rules_version
) VALUES (
    'codex-old', 'test-secret', 'definite', 'message',
    1, 0, 8, 0, '[redacted]', 'stale-rules'
)`)
	require.NoError(t, err, "seed stale PG secret finding")

	literalChildContent := "Inspect this token: " + fernet
	insertSession("codex-literal-child", "codex", "subagent", "literal", 64)
	insertMessage("codex-literal-child", 0, "user", literalChildContent, false)
	_, err = pg.ExecContext(ctx, `
UPDATE sessions
   SET quality_signal_version = 2, short_prompt_count = 9,
	   health_score = 91, health_grade = 'A',
       secret_leak_count = 1, secrets_rules_version = 'current-rules',
       transcript_revision = '11'
 WHERE id = 'codex-literal-child'`)
	require.NoError(t, err, "seed preserved literal PG derived data")
	_, err = pg.ExecContext(ctx, `
INSERT INTO secret_findings (
    session_id, rule_name, confidence, location_kind,
    message_ordinal, match_start, match_end, match_index,
    redacted_match, rules_version
) VALUES (
    'codex-literal-child', 'literal-secret', 'definite', 'message',
    0, 0, 8, 0, '[redacted]', 'current-rules'
)`)
	require.NoError(t, err, "seed preserved literal PG secret finding")

	rootQuote := "quoted " + fernet
	insertSession("codex-root", "codex", "", rootQuote, 64)
	insertMessage("codex-root", 0, "user", rootQuote, false)
	shellContent := "[Bash: decrypt]\n$ decrypt " + fernet
	shellInput := `{"command":"decrypt ` + fernet + `"}`
	insertSession("codex-shell", "codex", "", "shell session", 64)
	insertMessage("codex-shell", 0, "assistant", shellContent, true)
	_, err = pg.ExecContext(ctx, `
INSERT INTO tool_calls (
    session_id, tool_name, category, message_ordinal, input_json
) VALUES ($1, 'Bash', 'Shell', 0, $2)`, "codex-shell", shellInput)
	require.NoError(t, err, "insert non-collab tool call")
	longLookalikeContent := "[Task: send_message]\nliteral gAAAAA" +
		strings.Repeat("D", 48)
	insertSession("codex-long-lookalike", "codex", "", "literal tool", 64)
	insertMessage("codex-long-lookalike", 0, "assistant", longLookalikeContent, true)
	_, err = pg.ExecContext(ctx, `
INSERT INTO tool_calls (
    session_id, tool_name, category, message_ordinal, input_json
) VALUES ($1, 'send_message', 'Task', 0, '{}')`, "codex-long-lookalike")
	require.NoError(t, err, "insert long lookalike collab tool call")
	insertSession("other-old", "claude", "", fernet, 64)
	insertMessage("other-old", 0, "user", fernet, false)
	insertSession("codex-current", "codex", "subagent", fernet,
		codexEncryptedPayloadDataVersion)
	insertMessage("codex-current", 0, "user", fernet, false)
	multiTokenPreview := fernet + " " + fernet
	insertSession("codex-multi-preview", "codex", "subagent", multiTokenPreview, 64)

	genID, err := ensureVectorGeneration(ctx, pg, "codex-repair-fp", "test-model", 4)
	require.NoError(t, err, "register vector generation")
	require.NoError(t, ensureVectorChunkTable(ctx, pg, genID, 4),
		"create vector chunk table")
	for _, row := range []struct {
		docKey    string
		sessionID string
	}{
		{docKey: "old-doc", sessionID: "codex-old"},
		{docKey: "current-doc", sessionID: "codex-current"},
	} {
		_, err := pg.ExecContext(ctx, `
INSERT INTO vector_documents (
    doc_key, session_id, ordinal, ordinal_end, content, content_hash
) VALUES ($1, $2, 0, 0, $3, $4)`,
			row.docKey, row.sessionID, fernet, row.docKey+"-hash")
		require.NoError(t, err, "insert vector document %s", row.docKey)
		_, err = pg.ExecContext(ctx, `
INSERT INTO vector_push_state (generation_id, session_id, doc_agg_hash)
VALUES ($1, $2, $3)`, genID, row.sessionID, row.docKey+"-agg")
		require.NoError(t, err, "insert vector push state %s", row.docKey)
	}
	extSchema, err := vectorExtensionSchema(ctx, pg)
	require.NoError(t, err, "resolve vector extension schema")
	chunkTable := vectorChunkTable(genID)
	for _, docKey := range []string{"old-doc", "current-doc"} {
		_, err := pg.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (doc_key, chunk_index, embedding)
VALUES ($1, 0, $2::%s.halfvec)`, chunkTable, extSchema),
			docKey, "[1,0,0,0]")
		require.NoError(t, err, "insert vector chunk %s", docKey)
	}

	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"read-only compatibility gate must reject stale payloads")

	require.NoError(t, EnsureSchema(ctx, pg, codexRedactionTestSchema),
		"repair existing PG data")
	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"repaired PG data must pass the read-only gate")

	var repairedDataVersion int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT data_version FROM sessions WHERE id = 'codex-old'`,
	).Scan(&repairedDataVersion))
	assert.Equal(t, codexEncryptedPayloadDataVersion, repairedDataVersion)

	_, err = pg.ExecContext(ctx, `
UPDATE sessions SET data_version = 64 WHERE id = 'codex-current'`)
	require.Error(t, err,
		"a legacy session push after reader startup must be rejected")

	currentVectorTx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "begin current post-repair vector push")
	require.NoError(t, lockCodexVectorMutationSharedPG(ctx, currentVectorTx, false),
		"mark current post-repair vector push")
	_, err = currentVectorTx.ExecContext(ctx, `
INSERT INTO vector_documents (
    doc_key, session_id, ordinal, ordinal_end, content, content_hash
) VALUES ('post-guard-doc', 'codex-current', 0, 0, 'safe content', 'safe-hash')`)
	require.NoError(t, err, "insert current post-repair vector")
	require.NoError(t, currentVectorTx.Commit(), "commit current post-repair vector")
	_, err = pg.ExecContext(ctx, `
UPDATE vector_documents
   SET content = $1, content_hash = 'legacy-hash'
 WHERE doc_key = 'post-guard-doc'`, fernet)
	require.Error(t, err,
		"a legacy vector push after reader startup must be rejected")

	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"rejected legacy pushes must leave the PG reader compatible")
	_, err = pg.ExecContext(ctx,
		`DELETE FROM vector_documents WHERE doc_key = 'post-guard-doc'`)
	require.NoError(t, err, "remove post-startup guard probe vector")

	var gotContent string
	var gotLength int
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT content, content_length FROM messages
 WHERE session_id = 'codex-old' AND ordinal = 0`).Scan(&gotContent, &gotLength))
	assert.Equal(t, "[Task: spawn_agent]\n[encrypted]", gotContent)
	assert.Equal(t, len(gotContent), gotLength)

	var gotInbound string
	var gotInboundLength int
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT content, content_length FROM messages
 WHERE session_id = 'codex-old' AND ordinal = 1`).Scan(&gotInbound, &gotInboundLength))
	assert.Equal(t, "[encrypted]", gotInbound)
	assert.Equal(t, len(gotInbound), gotInboundLength)

	var gotLiteralChild string
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT content FROM messages
 WHERE session_id = 'codex-literal-child' AND ordinal = 0`).Scan(&gotLiteralChild))
	assert.Equal(t, literalChildContent, gotLiteralChild,
		"literal subagent text containing a token must survive")
	var literalQualityVersion, literalShortPrompts, literalSecretLeakCount int
	var literalHealthScore sql.NullInt64
	var literalHealthGrade sql.NullString
	var literalRulesVersion, literalTranscriptRevision string
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT quality_signal_version, short_prompt_count,
	   health_score, health_grade,
       secret_leak_count, secrets_rules_version, transcript_revision
  FROM sessions WHERE id = 'codex-literal-child'`).Scan(
		&literalQualityVersion, &literalShortPrompts,
		&literalHealthScore, &literalHealthGrade,
		&literalSecretLeakCount, &literalRulesVersion,
		&literalTranscriptRevision,
	), "query preserved literal PG derived data")
	assert.Equal(t, 2, literalQualityVersion)
	assert.Equal(t, 9, literalShortPrompts)
	assert.Equal(t, int64(91), literalHealthScore.Int64)
	assert.Equal(t, "A", literalHealthGrade.String)
	assert.Equal(t, 1, literalSecretLeakCount)
	assert.Equal(t, "current-rules", literalRulesVersion)
	assert.Equal(t, "11", literalTranscriptRevision,
		"an unchanged literal PG transcript must retain its revision")
	var literalFindingCount int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM secret_findings WHERE session_id = 'codex-literal-child'`,
	).Scan(&literalFindingCount), "count preserved literal PG secret findings")
	assert.Equal(t, 1, literalFindingCount)

	var qualityVersion, shortPrompts, missingSuccess, missingVerification int
	var duplicatePrompts, noCodeContext, runawayLoops, secretLeakCount int
	var unstructuredStart bool
	var healthScore sql.NullInt64
	var healthGrade sql.NullString
	var secretsRulesVersion, transcriptRevision string
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT quality_signal_version, health_score, health_grade,
	   short_prompt_count, unstructured_start,
       missing_success_criteria_count, missing_verification_count,
       duplicate_prompt_count, no_code_context_count,
       runaway_tool_loop_count, secret_leak_count, secrets_rules_version,
       transcript_revision
  FROM sessions WHERE id = 'codex-old'`).Scan(
		&qualityVersion, &healthScore, &healthGrade,
		&shortPrompts, &unstructuredStart,
		&missingSuccess, &missingVerification, &duplicatePrompts,
		&noCodeContext, &runawayLoops, &secretLeakCount,
		&secretsRulesVersion, &transcriptRevision,
	), "query invalidated PG derived data")
	assert.Equal(t, []int{0, 0, 0, 0, 0, 0, 0, 0}, []int{
		qualityVersion, shortPrompts, missingSuccess, missingVerification,
		duplicatePrompts, noCodeContext, runawayLoops, secretLeakCount,
	})
	assert.False(t, unstructuredStart)
	assert.False(t, healthScore.Valid)
	assert.False(t, healthGrade.Valid)
	assert.Empty(t, secretsRulesVersion)
	assert.Equal(t, "8", transcriptRevision,
		"the repaired PG transcript must advance exactly once")
	var findingCount int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM secret_findings WHERE session_id = 'codex-old'`,
	).Scan(&findingCount), "count invalidated PG secret findings")
	assert.Zero(t, findingCount)

	var gotInput string
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT input_json FROM tool_calls WHERE session_id = 'codex-old'`).Scan(&gotInput))
	assert.Equal(t, `{"task_name":"worker","message":"[encrypted]"}`, gotInput)

	var gotPreview string
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT first_message FROM sessions WHERE id = 'codex-old'`).Scan(&gotPreview))
	assert.Equal(t, "[encrypted]", gotPreview)

	// Older clients could concatenate more than one encrypted_content block
	// into a parser-owned subagent preview. The one-time migration repairs it.
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT first_message FROM sessions WHERE id = 'codex-multi-preview'`).Scan(&gotPreview))
	assert.Equal(t, "[encrypted] [encrypted]", gotPreview)

	var gotShellContent, gotShellInput string
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT m.content, tc.input_json
  FROM messages m
  JOIN tool_calls tc
    ON tc.session_id = m.session_id AND tc.message_ordinal = m.ordinal
 WHERE m.session_id = 'codex-shell'`).Scan(&gotShellContent, &gotShellInput))
	assert.Equal(t, shellContent, gotShellContent,
		"non-collab tool content must survive")
	assert.Equal(t, shellInput, gotShellInput,
		"non-collab tool input must survive")

	var gotLongLookalikeContent string
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT content FROM messages
 WHERE session_id = 'codex-long-lookalike' AND ordinal = 0`).Scan(
		&gotLongLookalikeContent,
	))
	assert.Equal(t, longLookalikeContent, gotLongLookalikeContent,
		"non-truncated Fernet-looking PG tool text must survive")

	for _, tc := range []struct {
		id   string
		want string
	}{
		{id: "codex-root", want: rootQuote},
		{id: "other-old", want: fernet},
		{id: "codex-current", want: fernet},
	} {
		var got string
		require.NoError(t, pg.QueryRowContext(ctx,
			`SELECT first_message FROM sessions WHERE id = $1`, tc.id).Scan(&got))
		assert.Equal(t, tc.want, got, tc.id)
	}

	for _, table := range []string{"vector_documents", "vector_push_state", chunkTable} {
		var oldCount, currentCount int
		keyColumn := "session_id"
		if table == chunkTable {
			keyColumn = "doc_key"
		}
		oldKey, currentKey := "codex-old", "codex-current"
		if table == chunkTable {
			oldKey, currentKey = "old-doc", "current-doc"
		}
		require.NoError(t, pg.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = $1", table, keyColumn),
			oldKey).Scan(&oldCount), "count repaired rows in %s", table)
		require.NoError(t, pg.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = $1", table, keyColumn),
			currentKey).Scan(&currentCount), "count current rows in %s", table)
		assert.Zero(t, oldCount, "old Codex vector state must be invalidated in %s", table)
		assert.Zero(t, currentCount,
			"the first unmarked sweep must invalidate every Codex vector in %s", table)
	}

	// Vector documents are pushed independently from session rows. Even after
	// the one-time sweep, stale parser-owned content on a current-version
	// session must fail the read-only gate and be invalidated by writable repair.
	execCurrentVectorMutation("push stale current-version vector document", `
INSERT INTO vector_documents (
    doc_key, session_id, ordinal, ordinal_end, content, content_hash
) VALUES ('current-doc', 'codex-current', 0, 0, $1, 'current-stale-hash')`, fernet)
	_, err = pg.ExecContext(ctx, `
INSERT INTO vector_push_state (generation_id, session_id, doc_agg_hash)
VALUES ($1, 'codex-current', 'current-stale-agg')`, genID)
	require.NoError(t, err, "push stale current-version vector state")
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (doc_key, chunk_index, embedding)
VALUES ('current-doc', 0, $1::%s.halfvec)`, chunkTable, extSchema), "[1,0,0,0]")
	require.NoError(t, err, "push stale current-version vector chunk")

	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"current session versions must not hide independently stale vectors")
	require.NoError(t, EnsureSchema(ctx, pg, codexRedactionTestSchema),
		"invalidate stale vectors for a current-version session")
	for _, table := range []string{"vector_documents", "vector_push_state", chunkTable} {
		keyColumn := "session_id"
		key := "codex-current"
		if table == chunkTable {
			keyColumn = "doc_key"
			key = "current-doc"
		}
		var count int
		require.NoError(t, pg.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = $1", table, keyColumn),
			key).Scan(&count), "count current-version stale rows in %s", table)
		assert.Zero(t, count,
			"stale current-version vector state must be invalidated in %s", table)
	}

	// A source-less orphan can remain at dataVersion 64 after its stored
	// content is repaired. Fresh vectors built from that scrubbed content are
	// valid and must not be treated as permanently stale on every startup.
	execCurrentVectorMutation("repopulate repaired vector document", `
INSERT INTO vector_documents (
    doc_key, session_id, ordinal, ordinal_end, content, content_hash
) VALUES ('old-doc', 'codex-old', 0, 0, '[encrypted]', 'rebuilt-hash')`)
	_, err = pg.ExecContext(ctx, `
INSERT INTO vector_push_state (generation_id, session_id, doc_agg_hash)
VALUES ($1, 'codex-old', 'rebuilt-agg')`, genID)
	require.NoError(t, err, "repopulate repaired legacy vector push state")
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (doc_key, chunk_index, embedding)
VALUES ('old-doc', 0, $1::%s.halfvec)`, chunkTable, extSchema), "[1,0,0,0]")
	require.NoError(t, err, "repopulate repaired legacy vector chunk")

	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"fresh vectors for a repaired legacy session must be compatible")
	require.NoError(t, EnsureSchema(ctx, pg, codexRedactionTestSchema),
		"repeat schema repair after fresh legacy vectors")
	for _, table := range []string{"vector_documents", "vector_push_state", chunkTable} {
		keyColumn := "session_id"
		if table == chunkTable {
			keyColumn = "doc_key"
		}
		key := "codex-old"
		if table == chunkTable {
			key = "old-doc"
		}
		var count int
		require.NoError(t, pg.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = $1", table, keyColumn),
			key).Scan(&count), "count rebuilt rows in %s", table)
		assert.Equal(t, 1, count,
			"fresh repaired-legacy vector state must survive in %s", table)
	}

	// A legacy client can push stale vectors after the one-time marker exists,
	// even when the stored messages are already clean. Read-only serving must
	// reject that state, and writable repair must invalidate only its vectors.
	execCurrentVectorMutation("push stale vector content after repair marker", `
UPDATE vector_documents SET content = $1, content_hash = 'late-stale-hash'
 WHERE doc_key = 'old-doc'`, fernet)
	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"stale vectors pushed after the marker must fail closed")
	require.NoError(t, EnsureSchema(ctx, pg, codexRedactionTestSchema),
		"invalidate stale vectors pushed after the repair marker")
	for _, table := range []string{"vector_documents", "vector_push_state", chunkTable} {
		keyColumn := "session_id"
		if table == chunkTable {
			keyColumn = "doc_key"
		}
		key := "codex-old"
		if table == chunkTable {
			key = "old-doc"
		}
		var count int
		require.NoError(t, pg.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = $1", table, keyColumn),
			key).Scan(&count), "count late-stale rows in %s", table)
		assert.Zero(t, count,
			"late stale vector state must be invalidated in %s", table)
	}

	// Restore clean vector state for the independent stored-message repair
	// scenario below.
	execCurrentVectorMutation("repopulate clean vector document", `
INSERT INTO vector_documents (
    doc_key, session_id, ordinal, ordinal_end, content, content_hash
) VALUES ('old-doc', 'codex-old', 0, 0, '[encrypted]', 'rebuilt-again-hash')`)
	_, err = pg.ExecContext(ctx, `
INSERT INTO vector_push_state (generation_id, session_id, doc_agg_hash)
VALUES ($1, 'codex-old', 'rebuilt-again-agg')`, genID)
	require.NoError(t, err, "repopulate clean vector push state")
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (doc_key, chunk_index, embedding)
VALUES ('old-doc', 0, $1::%s.halfvec)`, chunkTable, extSchema), "[1,0,0,0]")
	require.NoError(t, err, "repopulate clean vector chunk")

	// If a database predates or loses the session guard, the migration remains
	// able to repair a legacy payload and invalidate only that session's rebuilt
	// vectors before reinstalling the guard.
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
DROP TRIGGER %s ON sessions;
DROP TRIGGER %s ON messages`,
		codexSessionWriteGuardTrigger, codexMessageWriteGuardTrigger))
	require.NoError(t, err, "simulate a legacy schema without content guards")
	_, err = pg.ExecContext(ctx, `
UPDATE sessions SET data_version = 64 WHERE id = 'codex-old'`)
	require.NoError(t, err, "downgrade the simulated legacy session")
	_, err = pg.ExecContext(ctx, `
UPDATE messages SET content = $1, content_length = $2
 WHERE session_id = 'codex-old' AND ordinal = 1`, fernet, len(fernet))
	require.NoError(t, err, "restore stale content in the legacy schema")
	require.NoError(t, EnsureSchema(ctx, pg, codexRedactionTestSchema),
		"repair stale content and reinstall the write guard")
	for _, table := range []string{"vector_documents", "vector_push_state", chunkTable} {
		keyColumn := "session_id"
		if table == chunkTable {
			keyColumn = "doc_key"
		}
		key := "codex-old"
		if table == chunkTable {
			key = "old-doc"
		}
		var count int
		require.NoError(t, pg.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = $1", table, keyColumn),
			key).Scan(&count), "count re-invalidated rows in %s", table)
		assert.Zero(t, count,
			"vectors for content repaired after the marker must be invalidated in %s", table)
	}

	// sync_metadata is not required by CheckSchemaCompat for a read-only
	// pg serve role. Rebuild valid vector state for the still-v64 orphan, then
	// prove that missing metadata accepts it while actual stale vector content
	// still fails closed with the actionable compatibility sentinel.
	execCurrentVectorMutation("repopulate repaired vector document for restricted reader", `
INSERT INTO vector_documents (
    doc_key, session_id, ordinal, ordinal_end, content, content_hash
) VALUES ('old-doc', 'codex-old', 1, 1, '[encrypted]', 'restricted-reader-hash')`)
	_, err = pg.ExecContext(ctx, `
INSERT INTO vector_push_state (generation_id, session_id, doc_agg_hash)
VALUES ($1, 'codex-old', 'restricted-reader-agg')`, genID)
	require.NoError(t, err, "repopulate repaired vector push state for restricted reader")
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
INSERT INTO %s (doc_key, chunk_index, embedding)
VALUES ('old-doc', 0, $1::%s.halfvec)`, chunkTable, extSchema), "[1,0,0,0]")
	require.NoError(t, err, "repopulate repaired vector chunk for restricted reader")
	_, err = pg.ExecContext(ctx, `DROP TABLE sync_metadata`)
	require.NoError(t, err, "drop push-only sync metadata")
	require.NoError(t, CheckCodexEncryptedPayloadCompat(ctx, pg),
		"clean read-only schema without sync_metadata must remain compatible")

	insertSession("codex-grouped-vector", "codex", "", "grouped vector",
		codexEncryptedPayloadDataVersion)
	insertMessage("codex-grouped-vector", 0, "assistant",
		"[Task: spawn_agent]\n[encrypted]", true)
	insertMessage("codex-grouped-vector", 1, "assistant",
		"clean assistant follow-up", false)
	_, err = pg.ExecContext(ctx, `
INSERT INTO tool_calls (
    session_id, tool_name, category, message_ordinal, input_json
) VALUES (
    'codex-grouped-vector', 'spawn_agent', 'Task', 0,
    '{"task_name":"worker","message":"[encrypted]"}'
)`)
	require.NoError(t, err, "insert redacted grouped collab tool call")
	staleToolPreview := "[Task: spawn_agent]\n" +
		"gAAAAA" + strings.Repeat("A", 48) + "..."
	groupedContent := staleToolPreview + "\n\nclean assistant follow-up"
	groupedOffsets := fmt.Sprintf(
		`[{"o":0,"r":0,"b":0},{"o":1,"r":%d,"b":%d}]`,
		len([]rune(staleToolPreview))+2, len(staleToolPreview)+2,
	)
	execCurrentVectorMutation("insert grouped stale vector content", `
INSERT INTO vector_documents (
    doc_key, session_id, ordinal, ordinal_end, offsets, content, content_hash
) VALUES (
    'grouped-doc', 'codex-grouped-vector', 0, 1, $1, $2, 'grouped-stale-hash'
)`, groupedOffsets, groupedContent)
	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"grouped stale collab vector content without sync_metadata must fail closed")
	_, err = pg.ExecContext(ctx, `
DELETE FROM vector_documents WHERE doc_key = 'grouped-doc'`)
	require.NoError(t, err, "remove grouped stale vector content")

	execCurrentVectorMutation("restore stale vector content without sync metadata", `
UPDATE vector_documents SET content = $1, content_hash = 'stale-hash'
 WHERE doc_key = 'old-doc'`, fernet)
	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"stale data without sync_metadata must still fail closed")
}

func TestCheckCodexCompatRejectsScanModeMetadataOnPostgres(t *testing.T) {
	const schema = "agentsview_codex_scan_mode_pg_test"
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "connect to PG")
	defer pg.Close()
	t.Cleanup(func() {
		cleanup, cleanupErr := sql.Open("pgx", pgURL)
		require.NoError(t, cleanupErr, "connect for schema cleanup")
		defer cleanup.Close()
		_, cleanupErr = cleanup.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		require.NoError(t, cleanupErr, "drop test schema")
	})

	ctx := context.Background()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err, "reset test schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "create current schema")

	// Simulate a stale or copied CockroachDB metadata row on a PostgreSQL
	// server: the write guards are gone while the guard mode claims the
	// scan-based fallback. Scan mode is only legitimate on CockroachDB, so
	// the read gate must still fail closed here.
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
DROP TRIGGER %s ON sessions;
DROP TRIGGER %s ON messages;
DROP TRIGGER %s ON tool_calls;
DROP TRIGGER %s ON vector_documents`,
		codexSessionWriteGuardTrigger, codexMessageWriteGuardTrigger,
		codexToolWriteGuardTrigger, codexVectorWriteGuardTrigger))
	require.NoError(t, err, "simulate a schema without guards")
	_, err = pg.ExecContext(ctx, `
INSERT INTO sync_metadata (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		codexPayloadGuardModeMetadata, codexPayloadGuardModeScan)
	require.NoError(t, err, "force scan guard-mode metadata")

	err = CheckCodexEncryptedPayloadCompat(ctx, pg)
	require.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
		"scan-mode metadata on PostgreSQL must not weaken the fail-closed gate")
}

func TestCheckCodexCompatRejectsInactiveWriteGuard(t *testing.T) {
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	tests := []struct {
		name       string
		alterState string
	}{
		{name: "disabled", alterState: "DISABLE"},
		{name: "replica only", alterState: "ENABLE REPLICA"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := "agentsview_codex_inactive_guard_" +
				strings.ReplaceAll(tt.name, " ", "_") + "_test"
			pgURL := testPGURL(t)
			pg, err := Open(pgURL, schema, true)
			require.NoError(t, err, "connect to PG")
			defer pg.Close()
			t.Cleanup(func() {
				cleanup, cleanupErr := sql.Open("pgx", pgURL)
				require.NoError(t, cleanupErr, "connect for schema cleanup")
				defer cleanup.Close()
				_, cleanupErr = cleanup.Exec(
					"DROP SCHEMA IF EXISTS " + schema + " CASCADE",
				)
				require.NoError(t, cleanupErr, "drop test schema")
			})

			ctx := context.Background()
			_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
			require.NoError(t, err, "reset test schema")
			require.NoError(t, EnsureSchema(ctx, pg, schema), "create current schema")
			_, err = pg.ExecContext(ctx, fmt.Sprintf(
				"ALTER TABLE messages %s TRIGGER %s",
				tt.alterState, codexMessageWriteGuardTrigger,
			))
			require.NoError(t, err, "make message guard inactive for normal writes")

			installed, err := codexPayloadWriteGuardsInstalledPG(ctx, pg)
			require.NoError(t, err, "probe inactive guards")
			assert.False(t, installed,
				"an inactive trigger must not satisfy write-guard compatibility")
			for name, check := range map[string]func(context.Context, *sql.DB) error{
				"repair":     CheckCodexEncryptedPayloadCompat,
				"bounded":    CheckCodexEncryptedPayloadBoundedReadCompat,
				"persistent": CheckCodexEncryptedPayloadPersistentReadCompat,
			} {
				err = check(ctx, pg)
				assert.ErrorIs(t, err, ErrCodexEncryptedPayloadRepairRequired,
					"%s reads must fail closed", name)
			}

			_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version, first_message
) VALUES ('inactive-guard', 'test-machine', 'project', 'codex', 'subagent', 64, 'safe')`)
			require.NoError(t, err, "insert legacy session fixture")
			_, err = pg.ExecContext(ctx, `
INSERT INTO messages (session_id, ordinal, role, content, content_length)
VALUES ('inactive-guard', 0, 'user', $1, $2)`, fernet, len(fernet))
			require.NoError(t, err,
				"the inactive trigger demonstrates why readers must reject the schema")

			require.NoError(t, EnsureSchema(ctx, pg, schema),
				"a writable migration must reinstall the inactive guard")
			installed, err = codexPayloadWriteGuardsInstalledPG(ctx, pg)
			require.NoError(t, err, "probe reinstalled guards")
			assert.True(t, installed)
			require.NoError(t, CheckCodexEncryptedPayloadPersistentReadCompat(ctx, pg),
				"readers may resume after repair and guard reinstallation")
			var content string
			require.NoError(t, pg.QueryRowContext(ctx, `
SELECT content FROM messages
 WHERE session_id = 'inactive-guard' AND ordinal = 0`).Scan(&content))
			assert.Equal(t, "[encrypted]", content)
			_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, data_version, first_message
) VALUES ('inactive-guard-after', 'test-machine', 'project', 'codex', 'subagent', 64, 'safe')`)
			require.NoError(t, err, "insert a new legacy session after guard repair")
			_, err = pg.ExecContext(ctx, `
INSERT INTO messages (session_id, ordinal, role, content, content_length)
VALUES ('inactive-guard-after', 0, 'user', $1, $2)`, fernet, len(fernet))
			require.Error(t, err,
				"the reinstalled guard must reject later legacy ciphertext")
		})
	}
}

// A legacy row can mix a genuine Fernet token with content the redactors
// deliberately preserve (a short lookalike, a complete token quoted in
// literal preview text). The repaired row then still matches the write
// guards' LIKE prefilter, so the repair transaction's compatibility marker
// must let the scoped rewrites run before exhaustive certification promotes
// the session.
func TestEnsureSchemaRepairsMixedTokenAndLookalikeRows(t *testing.T) {
	const schema = "agentsview_codex_mixed_wedge_test"
	const fernet = "gAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	pgURL := testPGURL(t)
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "connect to PG")
	defer pg.Close()
	t.Cleanup(func() {
		cleanup, cleanupErr := sql.Open("pgx", pgURL)
		require.NoError(t, cleanupErr, "connect for schema cleanup")
		defer cleanup.Close()
		_, cleanupErr = cleanup.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		require.NoError(t, cleanupErr, "drop test schema")
	})

	ctx := context.Background()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err, "reset test schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "create current schema")
	_, err = pg.ExecContext(ctx, fmt.Sprintf(`
DROP TRIGGER %s ON sessions;
DROP TRIGGER %s ON messages;
DROP TRIGGER %s ON tool_calls;
DROP TRIGGER %s ON vector_documents`,
		codexSessionWriteGuardTrigger, codexMessageWriteGuardTrigger,
		codexToolWriteGuardTrigger, codexVectorWriteGuardTrigger))
	require.NoError(t, err, "simulate the schema of a legacy PG writer")
	_, err = pg.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key IN ($1, $2)`,
		codexVectorRepairCompletedMetadata, codexPayloadGuardModeMetadata)
	require.NoError(t, err, "clear migration metadata")

	// Preview: a complete quoted token the redactor preserves plus a
	// clipped tail it repairs, so the repair UPDATE writes a first_message
	// that still contains 'gAAAAA'.
	mixedPreview := "note " + fernet + " tail " + fernet[:60] + "..."
	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, first_message, data_version
) VALUES ('codex-mixed', 'test-machine', 'project', 'codex', 'subagent', $1, 64)`,
		mixedPreview)
	require.NoError(t, err, "insert mixed legacy session")
	// Message: a valid token next to a short lookalike the token regex
	// ignores, so the redacted content still matches LIKE '%gAAAAA%'.
	mixedContent := "[Task: spawn_agent]\n" + fernet + " see gAAAAAabc"
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('codex-mixed', 0, 'assistant', $1, $2, TRUE)`,
		mixedContent, len(mixedContent))
	require.NoError(t, err, "insert mixed legacy message")
	_, err = pg.ExecContext(ctx, `
INSERT INTO tool_calls (
    session_id, tool_name, category, message_ordinal, input_json
) VALUES ('codex-mixed', 'spawn_agent', 'Task', 0, $1)`,
		`{"task_name":"worker","message":"`+fernet+`","note":"gAAAAAabc"}`)
	require.NoError(t, err, "insert mixed legacy tool call")

	require.NoError(t, EnsureSchema(ctx, pg, schema),
		"migration must survive repaired rows that keep preserved lookalikes")

	var content, preview, input string
	var dataVersion int
	require.NoError(t, pg.QueryRowContext(ctx, `
SELECT s.first_message, s.data_version, m.content, tc.input_json
  FROM sessions s
  JOIN messages m ON m.session_id = s.id AND m.ordinal = 0
  JOIN tool_calls tc ON tc.session_id = s.id AND tc.message_ordinal = 0
 WHERE s.id = 'codex-mixed'`,
	).Scan(&preview, &dataVersion, &content, &input), "read repaired row")
	assert.Equal(t, "[Task: spawn_agent]\n[encrypted] see gAAAAAabc", content,
		"the valid token is redacted and the lookalike preserved")
	assert.Equal(t, "note "+fernet+" tail [encrypted]", preview,
		"the quoted token is preserved and the clipped tail repaired")
	assert.Equal(t,
		`{"task_name":"worker","message":"[encrypted]","note":"gAAAAAabc"}`,
		input, "the tool input keeps its lookalike")
	assert.Equal(t, codexEncryptedPayloadDataVersion, dataVersion,
		"the repaired session is promoted to the watermark")

	// The repair marker must not weaken the guards after the migration commits:
	// a fresh unlinked legacy ciphertext preview is still rejected.
	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, first_message, data_version
) VALUES ('codex-late', 'test-machine', 'project', 'codex', '', $1, 64)`,
		fernet)
	require.Error(t, err, "a late legacy ciphertext write must be rejected")
	assert.Contains(t, err.Error(), "encrypted payload",
		"rejection should come from the write guard")

	_, err = pg.ExecContext(ctx, `
INSERT INTO sessions (
    id, machine, project, agent, relationship_type, first_message, data_version
) VALUES ('codex-late-message', 'test-machine', 'project', 'codex', '', 'safe', 64)`)
	require.NoError(t, err, "insert clean legacy session for message guards")
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('codex-late-message', 0, 'user', $1, $2, FALSE)`,
		fernet, len(fernet))
	require.Error(t, err, "an unlinked legacy user ciphertext write must be rejected")
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('codex-late-message', 1, 'assistant', $1, $2, TRUE)`,
		fernet, len(fernet))
	require.Error(t, err,
		"a legacy tool-use ciphertext write without a tool row must be rejected")

	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('codex-late-message', 2, 'assistant', $1, $2, FALSE)`,
		fernet, len(fernet))
	require.NoError(t, err, "insert plain assistant token before flag transition")
	_, err = pg.ExecContext(ctx, `
UPDATE messages SET has_tool_use = TRUE
 WHERE session_id = 'codex-late-message' AND ordinal = 2`)
	require.Error(t, err,
		"changing has_tool_use must re-run the PostgreSQL message guard")
	formatted := "[Task: spawn_agent]\n" + fernet
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('codex-late-message', 3, 'assistant', $1, $2, FALSE)`,
		formatted, len(formatted))
	require.Error(t, err,
		"a formatted collaboration block must not need either metadata signal")
	bashFormatted := "[Bash: decrypt]\n$ decrypt " + fernet
	_, err = pg.ExecContext(ctx, `
INSERT INTO messages (
    session_id, ordinal, role, content, content_length, has_tool_use
) VALUES ('codex-late-message', 4, 'assistant', $1, $2, FALSE)`,
		bashFormatted, len(bashFormatted))
	require.NoError(t, err,
		"a formatted non-collaboration block must remain permitted")
}
