package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tempDBPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

func createClosedTestDB(
	t *testing.T,
	path string,
	seed func(*DB),
) string {
	t.Helper()
	d, err := openCopiedTestDB(path)
	require.NoError(t, err)
	if seed != nil {
		seed(d)
	}
	require.NoError(t, d.Close())
	return path
}

func copyClosedTestDB(t *testing.T, src string) string {
	t.Helper()
	dst := tempDBPath(t, filepath.Base(src))
	copyTestDBFile(t, src, dst, true)
	copyTestDBFile(t, src+"-wal", dst+"-wal", false)
	copyTestDBFile(t, src+"-shm", dst+"-shm", false)
	return dst
}

func copyTestDBFile(t *testing.T, src, dst string, required bool) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return
		}
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(dst, data, 0o600))
}

func openReadOnlyTestDB(t *testing.T, path string) *DB {
	t.Helper()
	readonly, err := OpenReadOnly(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })
	return readonly
}

func execRawSQLite(t *testing.T, path, query string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.Exec(query, args...)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
}

func requireOpenReadOnlyFails(
	t *testing.T,
	path string,
	contains string,
) {
	t.Helper()
	readonly, err := OpenReadOnly(path)
	require.Error(t, err)
	require.Nil(t, readonly)
	assert.Contains(t, err.Error(), contains)
}

func requireReadOnlyOp(t *testing.T, name string, op func() error) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Helper()
		require.ErrorIs(t, op(), ErrReadOnly)
	})
}

func testModelPricing(pattern string) ModelPricing {
	return ModelPricing{
		ModelPattern:         pattern,
		InputPerMTok:         1,
		OutputPerMTok:        2,
		CacheCreationPerMTok: 3,
		CacheReadPerMTok:     4,
	}
}

func TestOpenReadOnlyExistingDBDoesNotWrite(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), func(d *DB) {
		require.NoError(t, d.SetSyncState("read_only_probe", "before"))
	})

	before, err := os.Stat(path)
	require.NoError(t, err)

	readonly := openReadOnlyTestDB(t, path)
	assert.True(t, readonly.ReadOnly())

	got, err := readonly.GetSyncState("read_only_probe")
	require.NoError(t, err)
	assert.Equal(t, "before", got)

	err = readonly.SetSyncState("read_only_probe", "after")
	require.ErrorIs(t, err, ErrReadOnly)

	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.Size(), after.Size())
	assert.Equal(t, before.ModTime(), after.ModTime())
}

// TestOpenReadOnlyReaderRefusesWritesAtSQLiteLevel pins the read-only
// contract below the Go-level requireWritable guard: mattn/go-sqlite3 only
// honors mode=ro when the DSN carries a file: URI prefix, so a bare-path DSN
// silently handed out writable reader handles. A write attempted directly on
// the reader pool must fail inside SQLite itself.
func TestOpenReadOnlyReaderRefusesWritesAtSQLiteLevel(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
	readonly := openReadOnlyTestDB(t, path)

	_, err := readonly.rawReader().Exec(
		`INSERT INTO stats (key, value) VALUES ('ro_probe', 1)`)
	require.Error(t, err,
		"a read-only reader connection must refuse writes")
	assert.Contains(t, err.Error(), "readonly",
		"the refusal must be SQLite's readonly-database error, got: %v", err)
}

// TestOpenPathWithSpecialCharacters pins makeDSN's path escaping: SQLite
// percent-decodes file: URI paths and splits params at `?`, so a directory
// name containing a space and a literal %-hex sequence ("%41") would, raw,
// be decoded to a different path ("weArd dir") and fail to open. Both the
// writable and read-only opens must escape the path, and the read-only
// reader must still refuse writes.
func TestOpenPathWithSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "we%41rd dir")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "sessions.db")

	rw, err := Open(path)
	require.NoError(t, err,
		"writable Open must succeed on a path with %% and space")
	require.NoError(t, rw.SetSyncState("special_path_probe", "x"))
	require.NoError(t, rw.Close())

	_, err = os.Stat(path)
	require.NoError(t, err,
		"the database file must exist at the literal path, not a decoded one")

	readonly := openReadOnlyTestDB(t, path)
	got, err := readonly.GetSyncState("special_path_probe")
	require.NoError(t, err)
	assert.Equal(t, "x", got)

	_, err = readonly.rawReader().Exec(
		`INSERT INTO stats (key, value) VALUES ('ro_probe', 1)`)
	require.Error(t, err,
		"a read-only reader connection must refuse writes")
	assert.Contains(t, err.Error(), "readonly",
		"the refusal must be SQLite's readonly-database error, got: %v", err)
}

// TestOpenReadOnlyNonWALJournalMode pins that a current-schema database left
// in a non-WAL journal mode still opens read-only: the ro DSN must not carry
// _journal_mode=WAL, because PRAGMA journal_mode=WAL is a write and fails on
// a mode=ro connection. The reader adopts the file's DELETE journal mode and
// still refuses writes.
func TestOpenReadOnlyNonWALJournalMode(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), func(d *DB) {
		require.NoError(t, d.SetSyncState("journal_probe", "delete-mode"))
	})
	execRawSQLite(t, path, "PRAGMA journal_mode=DELETE")
	_, err := os.Stat(path + "-wal")
	require.ErrorIs(t, err, os.ErrNotExist,
		"test setup: DELETE journal mode must have removed the WAL file")

	readonly := openReadOnlyTestDB(t, path)
	assert.True(t, readonly.ReadOnly())

	got, err := readonly.GetSyncState("journal_probe")
	require.NoError(t, err)
	assert.Equal(t, "delete-mode", got)

	require.ErrorIs(t, readonly.SetSyncState("journal_probe", "x"), ErrReadOnly)
	_, err = readonly.rawReader().Exec(
		`INSERT INTO stats (key, value) VALUES ('ro_probe', 1)`)
	require.Error(t, err,
		"a read-only reader connection must refuse writes")
	assert.Contains(t, err.Error(), "readonly",
		"the refusal must be SQLite's readonly-database error, got: %v", err)
}

func TestOpenReadOnlyWriteMethodsReturnErrReadOnly(t *testing.T) {
	pricing := testModelPricing("model-a")
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), func(d *DB) {
		require.NoError(t, d.UpsertModelPricing([]ModelPricing{pricing}))
	})
	readonly := openReadOnlyTestDB(t, path)

	requireReadOnlyOp(t, "UpsertSession", func() error {
		return readonly.UpsertSession(Session{ID: "s", Agent: "codex"})
	})
	requireReadOnlyOp(t, "WriteSessionBatch", func() error {
		_, err := readonly.WriteSessionBatch(nil)
		return err
	})
	requireReadOnlyOp(t, "WriteSessionBatchAtomic", func() error {
		_, err := readonly.WriteSessionBatchAtomic(nil)
		return err
	})
	requireReadOnlyOp(t, "UpsertModelPricing nil", func() error {
		return readonly.UpsertModelPricing(nil)
	})
	requireReadOnlyOp(t, "UpsertModelPricing populated", func() error {
		return readonly.UpsertModelPricing([]ModelPricing{pricing})
	})
	requireReadOnlyOp(t, "InsertMessages", func() error {
		return readonly.InsertMessages(nil)
	})
	requireReadOnlyOp(t, "BulkStarSessions", func() error {
		return readonly.BulkStarSessions(nil)
	})
	requireReadOnlyOp(t, "DeleteParserExcludedSessions", func() error {
		_, err := readonly.DeleteParserExcludedSessions(nil)
		return err
	})
	requireReadOnlyOp(t, "DeleteSessions", func() error {
		_, err := readonly.DeleteSessions(nil)
		return err
	})
	requireReadOnlyOp(t, "InsertMissingModelPricing", func() error {
		return readonly.InsertMissingModelPricing([]ModelPricing{{
			ModelPattern: "x",
		}})
	})
	requireReadOnlyOp(t, "ReplaceSkippedFiles", func() error {
		return readonly.ReplaceSkippedFiles(map[string]int64{"x": 1})
	})
	requireReadOnlyOp(t, "ClearRemoteSkippedFiles", func() error {
		return readonly.ClearRemoteSkippedFiles("remote-host")
	})
	requireReadOnlyOp(t, "UpdateSessionIncremental", func() error {
		return readonly.UpdateSessionIncremental("s", IncrementalSessionUpdate{})
	})
	requireReadOnlyOp(t, "RecordRecallQueryEvent", func() error {
		_, err := readonly.RecordRecallQueryEvent(
			context.Background(), RecallQueryEvent{Surface: "query"},
		)
		return err
	})
}

func TestOpenReadOnlyRejectsMissingMigratedColumn(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
	execRawSQLite(t, path, "ALTER TABLE sessions DROP COLUMN display_name")
	requireOpenReadOnlyFails(t, path, "schema missing sessions.display_name")
}

func TestReadOnlySchemaCompatibilityRejectsMissingReadColumn(t *testing.T) {
	tests := []struct {
		name   string
		table  string
		column string
	}{
		{"session", "sessions", "session_name"},
		{"session file identity", "sessions", "file_inode"},
		{"session file device", "sessions", "file_device"},
		{"session file hash", "sessions", "file_hash"},
		{"session local modified", "sessions", "local_modified_at"},
		{"message", "messages", "source_subtype"},
		{"tool call", "tool_calls", "result_content"},
		{"tool result event", "tool_result_events", "content"},
		{"insight", "insights", "template_id"},
		{"pinned message", "pinned_messages", "note"},
		{"starred session", "starred_sessions", "created_at"},
		{"excluded session", "excluded_sessions", "created_at"},
		{"worktree mapping", "worktree_project_mappings", "updated_at"},
		{"pg sync state", "pg_sync_state", "value"},
		{"model pricing", "model_pricing", "updated_at"},
		{"secret finding", "secret_findings", "rules_version"},
		{"recall entry", "recall_entries", "uncertainty"},
		{"recall evidence", "recall_evidence", "snippet"},
		{"extract generation", "recall_extract_generations", "state"},
		{"extract progress stamp", "recall_extract_progress",
			"content_stamped_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := openReadOnlySchemaProbe(t)
			_, err := conn.Exec(
				"ALTER TABLE " + tt.table + " DROP COLUMN " + tt.column)
			require.NoError(t, err)
			requireReadOnlySchemaCompatibilityFails(t, conn,
				"schema missing "+tt.table+"."+tt.column)
		})
	}
}

func TestOpenReadOnlyRejectsMissingReadTable(t *testing.T) {
	basePath := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
	tests := []struct {
		table  string
		column string
	}{
		{"stats", "key"},
		{"usage_events", "id"},
		{"pinned_messages", "id"},
		{"secret_findings", "id"},
		{"pg_sync_state", "key"},
		{"model_pricing", "model_pattern"},
		{"recall_query_events", "id"},
		{"recall_query_exposures", "query_id"},
		{"recall_extract_generations", "fingerprint"},
		{"recall_extract_progress", "session_id"},
	}
	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			path := copyClosedTestDB(t, basePath)
			execRawSQLite(t, path, "DROP TABLE "+tt.table)
			requireOpenReadOnlyFails(t, path,
				"schema missing "+tt.table+"."+tt.column)
		})
	}
}

func TestReadOnlyRequiredSchemaDerivedFromSchemaDDL(t *testing.T) {
	required, err := readOnlyRequiredSchema()
	require.NoError(t, err)

	conn, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.Exec(schemaSQL)
	require.NoError(t, err)

	want := make(map[string][]string, len(readOnlyRequiredTables))
	for _, table := range readOnlyRequiredTables {
		want[table] = readOnlyTableColumns(t, conn, table)
	}

	assert.Equal(t, want, required)
}

func readOnlyTableColumns(
	t *testing.T,
	conn *sql.DB,
	table string,
) []string {
	t.Helper()
	rows, err := conn.Query(
		"SELECT name FROM pragma_table_info(?) ORDER BY cid", table,
	)
	require.NoError(t, err)
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, columns)
	return columns
}

func openReadOnlySchemaProbe(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.Exec(schemaSQL)
	require.NoError(t, err)
	return conn
}

func requireReadOnlySchemaCompatibilityFails(
	t *testing.T,
	conn *sql.DB,
	contains string,
) {
	t.Helper()
	err := checkReadOnlySchemaCompatibility(conn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), contains)
}

func TestOpenReadOnlyAllowsMissingFTSTable(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
	execRawSQLite(t, path, "DROP TRIGGER IF EXISTS messages_ai")
	execRawSQLite(t, path, "DROP TRIGGER IF EXISTS messages_au")
	execRawSQLite(t, path, "DROP TRIGGER IF EXISTS messages_ad")
	execRawSQLite(t, path, "DROP TABLE IF EXISTS messages_fts")

	readonly, err := OpenReadOnly(path)
	require.NoError(t, err)
	require.NotNil(t, readonly)
	defer readonly.Close()
	assert.False(t, readonly.HasFTS())
}

func TestOpenReadOnlyCopyHelpersReturnErrReadOnly(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "source.db")
	createClosedTestDB(t, srcPath, nil)

	dstPath := filepath.Join(dir, "dest.db")
	createClosedTestDB(t, dstPath, nil)
	readonly := openReadOnlyTestDB(t, dstPath)

	requireReadOnlyOp(t, "CopyInsightsFrom", func() error {
		return readonly.CopyInsightsFrom(srcPath)
	})
	requireReadOnlyOp(t, "CopyOrphanedDataFrom", func() error {
		_, err := readonly.CopyOrphanedDataFrom(srcPath)
		return err
	})
	requireReadOnlyOp(t, "CopyTrashedDataFrom", func() error {
		_, err := readonly.CopyTrashedDataFrom(srcPath)
		return err
	})
	requireReadOnlyOp(t, "CopySyncStateFrom", func() error {
		return readonly.CopySyncStateFrom(srcPath)
	})
	requireReadOnlyOp(t, "CopyExcludedSessionsFrom", func() error {
		return readonly.CopyExcludedSessionsFrom(srcPath)
	})
	requireReadOnlyOp(t, "CopySessionMetadataFrom", func() error {
		return readonly.CopySessionMetadataFrom(srcPath)
	})
	requireReadOnlyOp(t, "CopyWorktreeProjectMappingsFrom", func() error {
		return readonly.CopyWorktreeProjectMappingsFrom(srcPath)
	})
}

func TestOpenReadOnlyMissingDBFailsWithoutCreatingFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "sessions.db")

	readonly, err := OpenReadOnly(path)
	require.Error(t, err)
	require.Nil(t, readonly)

	_, statErr := os.Stat(path)
	require.ErrorIs(t, statErr, os.ErrNotExist)

	_, statErr = os.Stat(filepath.Dir(path))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestOpenReadOnlyEmptyDBFailsWithoutMigrating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	require.NoError(t, os.WriteFile(path, nil, 0o644))

	readonly, err := OpenReadOnly(path)
	require.Error(t, err)
	require.Nil(t, readonly)

	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.Zero(t, info.Size())
}
