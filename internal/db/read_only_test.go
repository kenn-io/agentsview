package db

import (
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
	d, err := Open(path)
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
	requireReadOnlyOp(t, "UpdateSessionIncremental", func() error {
		return readonly.UpdateSessionIncremental("s", IncrementalSessionUpdate{})
	})
}

func TestOpenReadOnlyRejectsMissingMigratedColumn(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
	execRawSQLite(t, path, "ALTER TABLE sessions DROP COLUMN display_name")
	requireOpenReadOnlyFails(t, path, "schema missing sessions.display_name")
}

func TestOpenReadOnlyRejectsMissingReadColumn(t *testing.T) {
	basePath := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := copyClosedTestDB(t, basePath)
			execRawSQLite(t, path,
				"ALTER TABLE "+tt.table+" DROP COLUMN "+tt.column)
			requireOpenReadOnlyFails(t, path,
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

func TestOpenReadOnlyAllowsMissingFTSTable(t *testing.T) {
	path := createClosedTestDB(t, tempDBPath(t, "sessions.db"), nil)
	execRawSQLite(t, path, "DROP TABLE messages_fts")

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
