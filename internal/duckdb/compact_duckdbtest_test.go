//go:build duckdbtest && !(windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// snapshotMirrorCounts opens the mirror file, reads the row count of every
// mirror table, and closes it again. The file must not be held open elsewhere.
func snapshotMirrorCounts(
	t *testing.T, ctx context.Context, path string,
) map[string]int64 {
	t.Helper()
	conn, err := Open(path)
	require.NoError(t, err, "open mirror for snapshot")
	defer func() {
		require.NoError(t, conn.Close(), "close mirror snapshot connection")
	}()
	return readMirrorCounts(t, ctx, conn)
}

func readMirrorCounts(
	t *testing.T, ctx context.Context, conn *sql.DB,
) map[string]int64 {
	t.Helper()
	counts := make(map[string]int64, len(mirrorTables))
	for _, table := range mirrorTables {
		var n int64
		require.NoError(t,
			conn.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM "+table.name,
			).Scan(&n),
			"count rows in %s", table.name,
		)
		counts[table.name] = n
	}
	return counts
}

func churnMirror(t *testing.T, ctx context.Context, local *db.DB, path string) {
	t.Helper()
	syncer, err := New(path, local, "test-machine", SyncOptions{})
	require.NoError(t, err, "open sync for churn")
	defer func() {
		require.NoError(t, syncer.Close(), "close churn sync")
	}()
	require.NoError(t, local.SetSyncState(
		syncer.transcriptRevisionBackfillKey(), "1",
	))
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err, "initial churn push")

	// Rewrite the alpha session several times. Each full re-push deletes and
	// re-inserts the session's rows, which is the delete/upsert + row-group
	// rewrite pattern that leaves allocated-but-free blocks behind.
	for i := 0; i < 5; i++ {
		ts := "2026-01-10T02:0" + string(rune('0'+i)) + ":00.000Z"
		_, err = local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
			Session: syncSession(
				"duck-sync-alpha", "alpha", "alpha churn", ts, 2,
			),
			Messages: []db.Message{
				syncMessage("duck-sync-alpha", 0, "user", "alpha churn", ts),
				syncMessage(
					"duck-sync-alpha", 1, "assistant",
					"alpha churn assistant body", ts,
					db.ToolCall{
						ToolName:  "search",
						Category:  "search",
						SkillName: "duck-search",
						ToolUseID: "tool-alpha",
						InputJSON: `{"query":"churn"}`,
						ResultEvents: []db.ToolResultEvent{{
							Source:        "tool",
							Status:        "complete",
							Content:       "churn result",
							Timestamp:     ts,
							EventIndex:    0,
							ContentLength: len("churn result"),
						}},
					},
				),
			},
			DataVersion:     i + 2,
			ReplaceMessages: true,
		}})
		require.NoError(t, err, "churn rewrite %d", i)
		_, err = syncer.Push(ctx, true, nil)
		require.NoError(t, err, "churn push %d", i)
	}
}

func TestCompactPreservesRowsAndProducesUsableFile(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")

	churnMirror(t, ctx, local, path)

	before := snapshotMirrorCounts(t, ctx, path)
	require.Equal(t, int64(2), before["sessions"], "fixture seeds two sessions")
	require.Greater(t, before["messages"], int64(0))

	result, err := Compact(ctx, path)
	require.NoError(t, err, "compact mirror")

	assert.Equal(t, path, result.Path)
	assert.Equal(t, len(mirrorTables), len(result.RowCounts))
	assert.Equal(t, before["sessions"], result.RowCounts["sessions"])
	assert.Equal(t, before["messages"], result.RowCounts["messages"])
	assert.Positive(t, result.BeforeFileBytes)
	assert.Positive(t, result.AfterFileBytes)

	after := snapshotMirrorCounts(t, ctx, path)
	assert.Equal(t, before, after, "every table must survive compaction")

	// No temp artifacts should be left behind.
	for _, suffix := range []string{".compact", ".compact.wal", ".wal"} {
		_, statErr := os.Stat(path + suffix)
		assert.Truef(t, os.IsNotExist(statErr),
			"expected %s to be absent after compaction", path+suffix)
	}

	// The compacted file must be usable through the read store.
	store, err := NewStore(path)
	require.NoError(t, err, "open compacted mirror as store")
	defer func() {
		require.NoError(t, store.Close(), "close store")
	}()
	page, err := store.ListSessions(ctx, db.SessionFilter{Limit: 10})
	require.NoError(t, err, "list sessions from compacted mirror")
	assert.Len(t, page.Sessions, 2)

	msgs, err := store.GetMessages(ctx, "duck-sync-alpha", 0, 10, true)
	require.NoError(t, err, "read messages from compacted mirror")
	assert.NotEmpty(t, msgs)

	stars, err := store.ListStarredSessionIDs(ctx)
	require.NoError(t, err, "read starred sessions from compacted mirror")
	assert.Equal(t, []string{"duck-sync-alpha"}, stars)
}

func TestCompactMissingFileReturnsClearError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "does-not-exist.duckdb")

	_, err := Compact(ctx, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr),
		"missing file must not be created by compact")
}

// TestCompactFailedCopyLeavesOriginalUntouched drives the failure path where
// the fresh file cannot be built (here the parent directory is read-only, so
// the temp destination cannot be created). The original must be byte- and
// row-identical afterward and no temp artifact may be left behind. This is the
// same "return before the swap, clean up temp" path a verification mismatch or
// an in-use lock on another process would take.
func TestCompactFailedCopyLeavesOriginalUntouched(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	seedDuckDBSyncFixture(t, local)
	dir := t.TempDir()
	path := filepath.Join(dir, "mirror.duckdb")

	churnMirror(t, ctx, local, path)

	info, err := os.Stat(path)
	require.NoError(t, err, "stat original before compact")
	beforeCounts := snapshotMirrorCounts(t, ctx, path)

	// Make the mirror's directory read-only so the temp destination file
	// cannot be created, forcing compaction to fail before any swap.
	require.NoError(t, os.Chmod(dir, 0o500), "make mirror dir read-only")
	t.Cleanup(func() {
		require.NoError(t, os.Chmod(dir, 0o700), "restore mirror dir perms")
	})

	_, err = Compact(ctx, path)
	require.Error(t, err, "compact must fail when the temp file cannot be built")

	// Restore write access so the follow-up assertions can reopen the file.
	require.NoError(t, os.Chmod(dir, 0o700), "restore mirror dir perms")

	afterInfo, err := os.Stat(path)
	require.NoError(t, err, "stat original after failed compact")
	assert.Equal(t, info.Size(), afterInfo.Size(),
		"original file size must be unchanged")
	assert.Equal(t, beforeCounts, snapshotMirrorCounts(t, ctx, path),
		"original rows must be unchanged after a failed compact")

	_, statErr := os.Stat(path + ".compact")
	assert.True(t, os.IsNotExist(statErr),
		"temp file must be cleaned up after a failed compact")
}
