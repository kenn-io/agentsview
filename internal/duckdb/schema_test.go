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

	"go.kenn.io/agentsview/internal/export"
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

	var version string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		schemaVersionMetadataKey,
	).Scan(&version))
	assert.Equal(t, strconv.Itoa(SchemaVersion), version)
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
	var remoteScrubbed string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT value FROM sync_metadata WHERE key = ?`,
		projectIdentityRemoteScrubMetadataKey,
	).Scan(&remoteScrubbed))
	assert.Equal(t, "1", remoteScrubbed)
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

func TestCheckSchemaCompatRejectsPendingNonIndexRepairs(t *testing.T) {
	ctx := context.Background()

	t.Run("messages id primary key", func(t *testing.T) {
		db := openTestDuckDB(t)
		require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
		recreateMessagesWithIDPrimaryKey(t, ctx, db)

		err := CheckSchemaCompat(ctx, db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "messages.id primary key")
	})

	t.Run("missing default repair metadata", func(t *testing.T) {
		db := openTestDuckDB(t)
		require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
		_, err := db.ExecContext(ctx,
			`DELETE FROM sync_metadata WHERE key = ?`,
			defaultRepairMetadataKey,
		)
		require.NoError(t, err)

		err = CheckSchemaCompat(ctx, db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), defaultRepairMetadataKey)
	})

	t.Run("missing usage repair metadata", func(t *testing.T) {
		db := openTestDuckDB(t)
		require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
		_, err := db.ExecContext(ctx,
			`DELETE FROM sync_metadata WHERE key = ?`,
			usageDedupIndexMetadataKey,
		)
		require.NoError(t, err)

		err = CheckSchemaCompat(ctx, db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), usageDedupIndexMetadataKey)
	})

	t.Run("missing project identity remote scrub metadata", func(t *testing.T) {
		db := openTestDuckDB(t)
		require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
		_, err := db.ExecContext(ctx,
			`DELETE FROM sync_metadata WHERE key = ?`,
			projectIdentityRemoteScrubMetadataKey,
		)
		require.NoError(t, err)

		err = CheckSchemaCompat(ctx, db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), projectIdentityRemoteScrubMetadataKey)
	})

	t.Run("quack incompatible timestamp default", func(t *testing.T) {
		db := openTestDuckDB(t)
		require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
		_, err := db.ExecContext(ctx,
			`ALTER TABLE starred_sessions ALTER COLUMN created_at SET DEFAULT current_timestamp`,
		)
		require.NoError(t, err)

		err = CheckSchemaCompat(ctx, db)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "starred_sessions.created_at")
	})
}

func TestEnsureSchemaScrubsProjectIdentityGitRemoteCredentials(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, db), "initial EnsureSchema")
	_, err := db.ExecContext(ctx,
		`DELETE FROM sync_metadata WHERE key = ?`,
		projectIdentityRemoteScrubMetadataKey,
	)
	require.NoError(t, err)
	rawRemote := "https://" + "user:token@" + "github.com/acme/app.git"
	storedRemote := "https://github.com/acme/app.git"
	_, err = db.ExecContext(ctx, `
		INSERT INTO source_project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"app", "laptop", "/tmp/app", rawRemote, "origin",
		"", "", "2026-07-03T12:00:00Z", "", "", "",
	)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO source_project_identity_observations (
			project, machine, root_path, git_remote, git_remote_name,
			worktree_name, worktree_root_path, observed_at,
			normalized_remote, key_source, key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"app", "laptop", "/tmp/app", "", "",
		"app", "/tmp/app", "2026-07-03T11:00:00Z", "", "", "",
	)
	require.NoError(t, err)

	require.NoError(t, EnsureSchema(ctx, db), "repair EnsureSchema")

	rows, err := db.QueryContext(ctx, `
		SELECT git_remote, normalized_remote, key_source, key
		FROM source_project_identity_observations
		WHERE project = ? AND machine = ? AND root_path = ?
		ORDER BY git_remote`,
		"app", "laptop", "/tmp/app",
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rows.Close()) })

	var got []export.ProjectIdentityObservation
	for rows.Next() {
		var obs export.ProjectIdentityObservation
		require.NoError(t, rows.Scan(
			&obs.GitRemote,
			&obs.NormalizedRemote,
			&obs.KeySource,
			&obs.Key,
		))
		got = append(got, obs)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 1)
	assert.Equal(t, storedRemote, got[0].GitRemote)
	assert.Equal(t, "github.com/acme/app", got[0].NormalizedRemote)
	assert.Equal(t, export.ProjectIdentityKeySourceGitRemote, got[0].KeySource)
	assert.NotEmpty(t, got[0].Key)
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
		"run agentsview duckdb push to migrate",
		"push cannot migrate a remote server schema")
}

func TestCheckSchemaCompatKeepsLocalMigrationHintOnMissingColumns(
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
	assert.Contains(t, err.Error(), "run agentsview duckdb push to migrate",
		"local mirrors migrate through push")
	assert.NotContains(t, err.Error(), "older AgentsView build")
}

func TestCheckSchemaCompatViaQuackReportsServerBehindOnOldVersion(
	t *testing.T,
) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")
	_, err := db.ExecContext(ctx,
		`UPDATE sync_metadata SET value = '1' WHERE key = ?`,
		schemaVersionMetadataKey,
	)
	require.NoError(t, err, "simulate older server schema version")

	err = CheckSchemaCompatViaQuack(ctx, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "older than required")
	assert.Contains(t, err.Error(), "upgrade and restart the DuckDB server",
		"remote version skew must tell the operator the fix")
}

// TestEnsureSchemaCreatesToolCallsFilePathIndex verifies the DuckDB mirror
// builds idx_tool_calls_file_path, the parity counterpart to SQLite's
// Recent Edits index. DuckDB has no partial indexes, so it omits the
// WHERE file_path IS NOT NULL clause but indexes the same column.
func TestEnsureSchemaCreatesToolCallsFilePathIndex(t *testing.T) {
	ctx := context.Background()
	db := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, db), "EnsureSchema")

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

func recreateMessagesWithIDPrimaryKey(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(ctx, `DROP TABLE messages`)
	require.NoError(t, err)
	create := strings.Replace(
		mirrorTableCreate("messages"),
		"id BIGINT,",
		"id BIGINT PRIMARY KEY,",
		1,
	)
	require.NotEqual(t, mirrorTableCreate("messages"), create)
	_, err = db.ExecContext(ctx, create)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO messages (id, session_id, ordinal, role, content)
		VALUES (1, 'from-other-machine', 0, 'user', 'kept')`)
	require.NoError(t, err)
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
