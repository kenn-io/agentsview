//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCreatesLocalDuckDBFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentsview.duckdb")

	db, err := Open(path)
	require.NoError(t, err, "Open")
	t.Cleanup(func() {
		require.NoError(t, db.Close(), "close DuckDB")
	})

	require.NoError(t, db.PingContext(context.Background()))
	assert.FileExists(t, path)
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	db, err := Open("")
	require.Error(t, err)
	assert.Nil(t, db)
	assert.Contains(t, err.Error(), "duckdb path is required")
}

func TestEnsureSchemaCreatesRequiredMirrorTables(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)

	require.NoError(t, createSchema(ctx, db), "createSchema")

	for _, table := range []string{
		"sync_metadata",
		"sessions",
		"messages",
		"usage_events",
		"cursor_usage_events",
		"model_pricing",
		"tool_calls",
		"tool_result_events",
		"secret_findings",
		"starred_sessions",
		"pinned_messages",
	} {
		assert.True(t, tableExists(t, db, table), "missing table %s", table)
	}
	assert.True(t, columnExists(t, db, "sessions", "agentsview_push_fingerprint"))

	var version string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	).Scan(&version))
	assert.Equal(t, strconv.Itoa(SchemaVersion), version)
}

func TestUsageEventsDedupIndexAllowsRepeatedKeys(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, createSchema(ctx, db), "createSchema")

	_, err := db.ExecContext(ctx, `
		INSERT INTO usage_events (id, session_id, source, model, dedup_key)
		VALUES
			(1, 's1', 'hermes', 'claude-test', ''),
			(2, 's1', 'hermes', 'claude-test', '')`)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `
		INSERT INTO usage_events (id, session_id, source, model, dedup_key)
		VALUES
			(3, 's1', 'hermes', 'claude-test', 'same-key'),
			(4, 's1', 'hermes', 'claude-test', 'same-key')`)
	require.NoError(t, err)
}

func TestEnsureSchemaIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)

	require.NoError(t, createSchema(ctx, db), "first createSchema")
	require.NoError(t, createSchema(ctx, db), "second createSchema")

	assert.True(t, columnExists(t, db, "sessions", "secret_leak_count"))
	assert.True(t, columnExists(t, db, "messages", "thinking_text"))
}

func TestCheckSchemaCompatReportsMissingTablesAndColumns(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)

	err := CheckSchemaCompat(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing table sessions")

	db = openTestDuckDB(t)
	_, err = db.ExecContext(ctx,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			machine TEXT NOT NULL,
			project TEXT NOT NULL,
			agent TEXT NOT NULL
		)`,
	)
	require.NoError(t, err)

	err = CheckSchemaCompat(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sessions.secret_leak_count")
}

func TestCheckSchemaCompatPassesAfterCreateSchema(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)

	require.NoError(t, createSchema(ctx, db), "createSchema")
	require.NoError(t, CheckSchemaCompat(ctx, db), "CheckSchemaCompat")
}

func TestCheckSchemaCompatViaQuackReportsServerBehindOnMissingColumns(
	t *testing.T,
) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	_, err := db.ExecContext(ctx,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			machine TEXT NOT NULL,
			project TEXT NOT NULL,
			agent TEXT NOT NULL
		)`,
	)
	require.NoError(t, err, "simulate older server schema")

	err = CheckSchemaCompatViaQuack(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sessions.entrypoint")
	assert.Contains(t, err.Error(), "older AgentsView build",
		"remote incompat must point at the server build")
	assert.Contains(t, err.Error(), "upgrade and restart the DuckDB server",
		"remote incompat must tell the operator the fix")
	assert.NotContains(t, err.Error(),
		"rebuild with 'agentsview duckdb push --full'",
		"push cannot migrate a remote server schema")
}

func TestCheckSchemaCompatKeepsLocalRebuildHintOnMissingColumns(
	t *testing.T,
) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	_, err := db.ExecContext(ctx,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			machine TEXT NOT NULL,
			project TEXT NOT NULL,
			agent TEXT NOT NULL
		)`,
	)
	require.NoError(t, err, "simulate stale local mirror")

	err = CheckSchemaCompat(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sessions.entrypoint")
	assert.Contains(t, err.Error(), "rebuild with 'agentsview duckdb push --full'",
		"local mirrors are rebuilt, not migrated in place")
	assert.NotContains(t, err.Error(), "older AgentsView build")
}

// TestCheckSchemaCompatRejectsSchemaVersionMismatchInBothDirections verifies
// that mirror schema v3's create-only version check rejects a mismatch in
// either direction (an older or a newer schema_version row than this
// build's SchemaVersion), for both the local mirror file and a remote Quack
// server, with the same rebuild hint.
func TestCheckSchemaCompatRejectsSchemaVersionMismatchInBothDirections(
	t *testing.T,
) {
	ctx := context.Background()
	checks := map[string]func(context.Context, *sql.DB) error{
		"local":  CheckSchemaCompat,
		"remote": CheckSchemaCompatViaQuack,
	}
	versions := map[string]string{
		"older": "1",
		"newer": strconv.Itoa(SchemaVersion + 1),
	}
	for versionName, version := range versions {
		for locationName, check := range checks {
			t.Run(versionName+"/"+locationName, func(t *testing.T) {
				db := openTestDuckDB(t)
				require.NoError(t, createSchema(ctx, db), "createSchema")
				_, err := db.ExecContext(ctx,
					`UPDATE sync_metadata SET value = ? WHERE key = ?`,
					version, schemaVersionMetadataKey,
				)
				require.NoError(t, err, "simulate mismatched schema version")

				err = check(ctx, db)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "does not match this build's")
				assert.Contains(t, err.Error(),
					"rebuild with 'agentsview duckdb push --full'")
			})
		}
	}
}

func TestCheckSchemaCompatViaQuackReportsServerBehindOnMissingVersionRow(
	t *testing.T,
) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, createSchema(ctx, db), "createSchema")
	_, err := db.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	)
	require.NoError(t, err, "simulate server without a schema version row")

	err = CheckSchemaCompatViaQuack(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), schemaVersionMetadataKey)
	assert.Contains(t, err.Error(), "upgrade and restart the DuckDB server",
		"remote missing version row must tell the operator the fix")
}

// TestEnsureSchemaCreatesToolCallsFilePathIndex verifies the DuckDB mirror
// builds idx_tool_calls_file_path, the parity counterpart to SQLite's
// Recent Edits index. DuckDB has no partial indexes, so it omits the
// WHERE file_path IS NOT NULL clause but indexes the same column.
func TestEnsureSchemaCreatesToolCallsFilePathIndex(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, createSchema(ctx, db), "createSchema")

	var count int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM duckdb_indexes()
		WHERE table_name = 'tool_calls'
		  AND index_name = 'idx_tool_calls_file_path'`).Scan(&count),
		"query duckdb_indexes")
	assert.Equal(t, 1, count,
		"idx_tool_calls_file_path must exist for Recent Edits parity")
}

func openTestDuckDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDuckDB("")
	require.NoError(t, err, "open in-memory DuckDB")
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	require.NoError(t, configureDuckDBThreads(db))
	t.Cleanup(func() {
		require.NoError(t, db.Close(), "close DuckDB")
	})
	return db
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT count(*) > 0
		 FROM information_schema.tables
		 WHERE table_schema = current_schema()
		   AND table_name = ?`,
		strings.ToLower(table),
	).Scan(&exists))
	return exists
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT count(*) > 0
		 FROM information_schema.columns
		 WHERE table_schema = current_schema()
		   AND table_name = ?
		   AND column_name = ?`,
		strings.ToLower(table),
		strings.ToLower(column),
	).Scan(&exists))
	return exists
}
