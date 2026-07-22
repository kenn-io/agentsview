//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const schemaTestSchema = "agentsview_schema_test"

func cleanSchemaTestPG(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()
	_, _ = pg.Exec(
		"DROP SCHEMA IF EXISTS " + schemaTestSchema + " CASCADE",
	)
}

func TestSessionDeletionCauseSchemaMigrationPreservesRows(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })
	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err)
	defer pg.Close()
	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))

	var exists bool
	require.NoError(t, pg.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'sessions'
			  AND column_name = 'deletion_cause'
		)`, schemaTestSchema).Scan(&exists))
	require.True(t, exists, "fresh schema must include sessions.deletion_cause")
	_, err = pg.Exec(`
		INSERT INTO sessions (id, machine, project, agent, deleted_at)
		VALUES ('preserved-trash', 'machine', 'project', 'claude', NOW())`)
	require.NoError(t, err)
	_, err = pg.Exec(`ALTER TABLE sessions DROP COLUMN deletion_cause`)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(t.Context(), pg, schemaTestSchema))
	var deleted bool
	var cause sql.NullString
	require.NoError(t, pg.QueryRow(`
		SELECT deleted_at IS NOT NULL, deletion_cause
		FROM sessions WHERE id = 'preserved-trash'`,
	).Scan(&deleted, &cause))
	assert.True(t, deleted, "migration must preserve legacy user trash")
	assert.False(t, cause.Valid, "legacy user trash must retain a NULL cause")
}

// TestSecretFindingsSchema verifies that EnsureSchema creates the
// secret_findings table with all required columns, and that the
// sessions table has the secret_leak_count and
// secrets_rules_version columns. Also asserts idempotency.
func TestSecretFindingsSchema(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()

	// Run EnsureSchema twice to verify idempotency.
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema),
		"EnsureSchema (first)")
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema),
		"EnsureSchema (second, idempotency check)")

	// Verify secret_findings table exists.
	var tableExists bool
	err = pg.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1
			  AND table_name = 'secret_findings'
		)`, schemaTestSchema).Scan(&tableExists)
	require.NoError(t, err, "checking secret_findings table")
	require.True(t, tableExists, "secret_findings table does not exist")

	// Verify all required columns on secret_findings.
	requiredFindingsCols := []string{
		"id", "session_id", "rule_name", "confidence",
		"location_kind", "message_ordinal", "call_index",
		"event_index", "match_start", "match_end",
		"match_index", "redacted_match", "rules_version",
		"created_at",
	}
	for _, col := range requiredFindingsCols {
		var exists bool
		err = pg.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1
				  AND table_name = 'secret_findings'
				  AND column_name = $2
			)`, schemaTestSchema, col).Scan(&exists)
		require.NoError(t, err, "checking secret_findings.%s", col)
		assert.True(t, exists, "secret_findings.%s column missing", col)
	}

	// Verify sessions has both secret-scan state columns.
	requiredSessionCols := []string{
		"secret_leak_count",
		"secrets_rules_version",
	}
	for _, col := range requiredSessionCols {
		var exists bool
		err = pg.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1
				  AND table_name = 'sessions'
				  AND column_name = $2
			)`, schemaTestSchema, col).Scan(&exists)
		require.NoError(t, err, "checking sessions.%s", col)
		assert.True(t, exists, "sessions.%s column missing", col)
	}
}

// TestToolCallsFilePathIndex verifies EnsureSchema creates the partial
// idx_tool_calls_file_path index that backs the cross-session Recent Edits
// feed, mirroring SQLite's index so the query surface has parity on PG.
func TestToolCallsFilePathIndex(t *testing.T) {
	pgURL := testPGURL(t)
	cleanSchemaTestPG(t, pgURL)
	t.Cleanup(func() { cleanSchemaTestPG(t, pgURL) })

	pg, err := Open(pgURL, schemaTestSchema, true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()
	// Twice to confirm CREATE INDEX IF NOT EXISTS stays idempotent.
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema), "EnsureSchema (first)")
	require.NoError(t, EnsureSchema(ctx, pg, schemaTestSchema), "EnsureSchema (second)")

	var exists bool
	err = pg.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = $1
			  AND tablename = 'tool_calls'
			  AND indexname = 'idx_tool_calls_file_path'
		)`, schemaTestSchema).Scan(&exists)
	require.NoError(t, err, "checking idx_tool_calls_file_path")
	assert.True(t, exists, "idx_tool_calls_file_path index missing")
}
