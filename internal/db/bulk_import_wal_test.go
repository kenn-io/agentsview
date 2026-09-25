package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBulkImportWALAutocheckpointPages(t *testing.T) {
	assert.Equal(t, 32768, bulkImportWALAutocheckpointPages(4096))
	assert.Equal(t, 16384, bulkImportWALAutocheckpointPages(8192))
	assert.Equal(t, 2048, bulkImportWALAutocheckpointPages(65536))
}

// TestBulkImportDefersWALCheckpointsOnReplacementOnly writes more than the
// default 1,000-page trigger into a live and a bulk-mode archive. Only the
// bulk archive keeps every frame in its WAL until FinishBulkImport.
func TestBulkImportDefersWALCheckpointsOnReplacementOnly(t *testing.T) {
	prev := bulkImportWALAutocheckpointBytes
	bulkImportWALAutocheckpointBytes = 16 << 20
	t.Cleanup(func() { bulkImportWALAutocheckpointBytes = prev })

	for _, pageSize := range []int{4096, 8192} {
		t.Run("page_size="+itoa(pageSize), func(t *testing.T) {
			live := testDB(t)
			replacementPath := filepath.Join(t.TempDir(), "replacement.db")
			createEmptyArchiveWithPageSize(t, replacementPath, pageSize)
			replacement, err := Open(t.Context(), replacementPath)
			require.NoError(t, err)
			reopened := false
			t.Cleanup(func() {
				if !reopened {
					require.NoError(t, replacement.Close())
				}
			})
			require.Equal(t, pageSize, writerPragma(t, replacement, "page_size"))

			require.NoError(t, replacement.DropBulkImportIndexes(t.Context()))
			assert.Equal(t, 16<<20/pageSize,
				writerPragma(t, replacement, "wal_autocheckpoint"))
			assert.Equal(t, 1000, writerPragma(t, live, "wal_autocheckpoint"),
				"the live writer keeps its policy")

			liveFrames := fillWAL(t, live, pageSize)
			replacementFrames := fillWAL(t, replacement, pageSize)
			assert.Greater(t, replacementFrames, 1000,
				"bulk mode must defer checkpoints past the default trigger")
			assert.Less(t, liveFrames, replacementFrames,
				"the live writer must still checkpoint at its default trigger")

			require.NoError(t, replacement.FinishBulkImport(t.Context()))
			assert.Equal(t, 1000,
				writerPragma(t, replacement, "wal_autocheckpoint"))
			info, err := os.Stat(replacementPath + "-wal")
			require.NoError(t, err)
			assert.Zero(t, info.Size(), "finish must truncate the WAL")

			require.NoError(t, replacement.Close())
			reopened = true
			again, err := Open(t.Context(), replacementPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, again.Close()) })
			assert.Equal(t, 1000, writerPragma(t, again, "wal_autocheckpoint"))
		})
	}
}

func createEmptyArchiveWithPageSize(t *testing.T, path string, pageSize int) {
	t.Helper()
	raw, err := sql.Open(sqliteArchiveDriverName, path)
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	for _, stmt := range []string{
		"PRAGMA page_size = " + itoa(pageSize),
		"CREATE TABLE page_size_seed(x)",
		"DROP TABLE page_size_seed",
	} {
		_, err := raw.ExecContext(t.Context(), stmt)
		require.NoError(t, err)
	}
}

func writerPragma(t *testing.T, database *DB, name string) int {
	t.Helper()
	var got int
	require.NoError(t, database.getWriter().QueryRow(t.Context(),
		"PRAGMA "+name).Scan(&got))
	return got
}

// fillWAL commits about 1,600 pages in small transactions and returns the
// frame count left in the WAL, which an automatic checkpoint would reset.
func fillWAL(t *testing.T, database *DB, pageSize int) int {
	t.Helper()
	w := database.getWriter()
	_, err := w.Exec(t.Context(), "CREATE TABLE wal_probe(b BLOB)")
	require.NoError(t, err)
	for range 20 {
		_, err := w.Exec(t.Context(),
			`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 40)
			 INSERT INTO wal_probe(b) SELECT randomblob(?) FROM n`, pageSize)
		require.NoError(t, err)
	}
	var busy, frames, checkpointed int
	require.NoError(t, w.QueryRow(t.Context(),
		"PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &frames, &checkpointed))
	return frames
}
