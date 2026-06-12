//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"path/filepath"
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

	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

	for _, table := range []string{
		"sync_metadata",
		"sessions",
		"messages",
		"usage_events",
		"model_pricing",
		"tool_calls",
		"tool_result_events",
		"secret_findings",
		"starred_sessions",
		"pinned_messages",
	} {
		assert.True(t, tableExists(t, db, table), "missing table %s", table)
	}

	var version string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	).Scan(&version))
	assert.Equal(t, "1", version)
	var repaired string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		defaultRepairMetadataKey,
	).Scan(&repaired))
	assert.Equal(t, "1", repaired)
	var dedupIndex string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		usageDedupIndexMetadataKey,
	).Scan(&dedupIndex))
	assert.Equal(t, "1", dedupIndex)
}

func TestUsageEventsDedupIndexAllowsRepeatedKeys(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

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

	require.NoError(t, EnsureSchema(ctx, db), "first EnsureSchema")
	require.NoError(t, EnsureSchema(ctx, db), "second EnsureSchema")

	assert.True(t, columnExists(t, db, "sessions", "secret_leak_count"))
	assert.True(t, columnExists(t, db, "messages", "thinking_text"))
}

func TestEnsureSchemaAddsMissingColumnsNonDestructively(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	_, err := db.ExecContext(ctx,
		`CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			machine TEXT NOT NULL,
			project TEXT NOT NULL,
			agent TEXT NOT NULL,
			message_count INTEGER,
			relationship_type TEXT,
			is_automated BOOLEAN
		)`,
	)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO sessions (
			id, machine, project, agent,
			message_count, relationship_type, is_automated
		) VALUES (?, ?, ?, ?, NULL, NULL, NULL)`,
		"kept", "mac", "alpha", "claude",
	)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

	assert.True(t, columnExists(t, db, "sessions", "ended_at"))
	var project string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT project FROM sessions WHERE id = ?`, "kept",
	).Scan(&project))
	assert.Equal(t, "alpha", project)
	var messageCount int
	var relationshipType string
	var isAutomated bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT message_count, relationship_type, is_automated
		 FROM sessions WHERE id = ?`, "kept",
	).Scan(&messageCount, &relationshipType, &isAutomated))
	assert.Equal(t, 0, messageCount)
	assert.Equal(t, "", relationshipType)
	assert.False(t, isAutomated)
}

func TestEnsureSchemaMigratesMessagesIDPrimaryKey(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	_, err := db.ExecContext(ctx, `
		CREATE TABLE messages (
			id BIGINT PRIMARY KEY,
			session_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			UNIQUE(session_id, ordinal)
		)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, ordinal, role, content)
		VALUES (1, 'from-other-machine', 0, 'user', 'kept')`)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

	hasPrimary, err := tableHasPrimaryKey(ctx, db, "messages")
	require.NoError(t, err)
	assert.False(t, hasPrimary)
	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, ordinal, role, content)
		VALUES (1, 'from-this-machine', 0, 'user', 'same local rowid')`)
	require.NoError(t, err)
	assertDuckDBCountWhere(t, db, "messages", "id = ?", int64(1), 2)
}

func TestEnsureSchemaDropsQuackIncompatibleTimestampDefaults(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	_, err := db.ExecContext(ctx, `
		CREATE TABLE starred_sessions (
			session_id TEXT PRIMARY KEY,
			created_at TIMESTAMP NOT NULL DEFAULT current_timestamp
		)`)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

	assert.NotContains(
		t,
		strings.ToLower(columnDefaultValue(t, db, "starred_sessions", "created_at")),
		"current_timestamp",
	)
	_, err = db.ExecContext(ctx,
		`INSERT INTO starred_sessions (session_id, created_at)
		 VALUES (?, current_timestamp)`,
		"kept",
	)
	require.NoError(t, err)
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

func TestCheckSchemaCompatPassesAfterEnsureSchema(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)

	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
	require.NoError(t, CheckSchemaCompat(ctx, db), "CheckSchemaCompat")
}

func TestSchemaDocumentsDuckDBDifferences(t *testing.T) {
	for _, want := range []string{
		"DuckDB stores timestamps as TIMESTAMP",
		"DuckDB BOOLEAN columns scan into Go bools",
		"SQLite INTEGER PRIMARY KEY rowids are mirrored as BIGINT",
		"DuckDB does not support SQLite FTS5 or PostgreSQL GIN indexes here",
		"DuckDB Quack rejects TIMESTAMP DEFAULT current_timestamp columns",
	} {
		assert.Contains(t, duckDBSchemaNotes, want)
	}
}

func openTestDuckDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "agentsview.duckdb"))
	require.NoError(t, err, "Open")
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

func columnDefaultValue(t *testing.T, db *sql.DB, table, column string) string {
	t.Helper()
	value, err := columnDefault(context.Background(), db, table, column)
	require.NoError(t, err)
	return value
}
