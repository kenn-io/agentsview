package remotesync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

// cancelAfterWriter cancels its context after the first write so the next
// write observes cancellation, as when a signal arrives mid-transfer.
type cancelAfterWriter struct {
	cancel context.CancelFunc
	writes int
}

func (w *cancelAfterWriter) Write(p []byte) (int, error) {
	w.writes++
	w.cancel()
	return len(p), nil
}

func TestWriteArchiveStopsWhenContextCancelled(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name),
			[]byte(strings.Repeat("x", 64<<10)), 0o644))
	}
	ctx, cancel := context.WithCancel(t.Context())
	out := &cancelAfterWriter{cancel: cancel}

	err := WriteArchive(ctx, out, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {dir}},
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, out.writes)
}

func TestSQLiteSnapshotStopsBetweenBackupSteps(t *testing.T) {
	src := filepath.Join(t.TempDir(), "state.db")
	conn, err := sql.Open("sqlite3", src)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `CREATE TABLE blobs (data BLOB)`)
	require.NoError(t, err)
	// Enough pages for several backup steps at the default page size.
	for range 40 {
		_, err = conn.ExecContext(t.Context(), `INSERT INTO blobs VALUES (randomblob(1 << 20))`)
		require.NoError(t, err)
	}
	require.NoError(t, conn.Close())

	ctx, cancel := context.WithCancel(t.Context())
	steps := 0
	afterSQLiteBackupStep = func() {
		steps++
		cancel()
	}
	t.Cleanup(func() { afterSQLiteBackupStep = nil })
	dst := filepath.Join(t.TempDir(), "snapshot.db")

	err = writeSQLiteSnapshot(ctx, dst, src)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, steps)
	_, statErr := os.Stat(dst)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestSQLiteSnapshotCompletesWhileSourceIsWritten(t *testing.T) {
	tests := []struct {
		name        string
		journalMode string
	}{
		{name: "wal", journalMode: "WAL"},
		{name: "rollback journal", journalMode: "DELETE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := filepath.Join(t.TempDir(), "state.db")
			const initialRows = 40
			writer := newSnapshotSourceDB(ctx, t, src, tt.journalMode, initialRows)

			// One write from a second connection after every step. Without a
			// pinned read snapshot each write restarts the backup, so stop a
			// runaway copy well past the ~10 steps this database needs.
			steps, writes := 0, 0
			afterSQLiteBackupStep = func() {
				steps++
				if steps > 200 {
					cancel()
					return
				}
				_, err := writer.ExecContext(ctx, `INSERT INTO blobs VALUES (randomblob(64))`)
				if err == nil {
					writes++
					return
				}
				// A rollback-journal writer cannot commit while the backup
				// holds its shared lock.
				var sqliteErr sqlite3.Error
				require.ErrorAs(t, err, &sqliteErr)
				require.Equal(t, sqlite3.ErrBusy, sqliteErr.Code)
			}
			t.Cleanup(func() { afterSQLiteBackupStep = nil })
			dst := filepath.Join(t.TempDir(), "snapshot.db")

			require.NoError(t, writeSQLiteSnapshot(ctx, dst, src))

			assert.Less(t, steps, 200)
			if tt.journalMode == "WAL" {
				assert.Positive(t, writes, "WAL writers keep committing during the backup")
			}
			snapshot, err := sql.Open("sqlite3", sqliteReadOnlyDSN(dst))
			require.NoError(t, err)
			defer snapshot.Close()
			var integrity string
			require.NoError(t, snapshot.QueryRowContext(ctx,
				`PRAGMA integrity_check`).Scan(&integrity))
			assert.Equal(t, "ok", integrity)
			var rows int
			require.NoError(t, snapshot.QueryRowContext(ctx,
				`SELECT count(*) FROM blobs`).Scan(&rows))
			assert.Equal(t, initialRows, rows, "snapshot reflects the source when the backup began")
		})
	}
}

// newSnapshotSourceDB creates a multi-step source database and returns a
// second, writable connection to it that fails fast instead of waiting on
// locks.
func newSnapshotSourceDB(
	ctx context.Context, t *testing.T, path, journalMode string, rows int,
) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=0&_journal_mode="+journalMode)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `CREATE TABLE blobs (data BLOB)`)
	require.NoError(t, err)
	for range rows {
		_, err = db.ExecContext(ctx, `INSERT INTO blobs VALUES (randomblob(1 << 20))`)
		require.NoError(t, err)
	}
	var mode string
	require.NoError(t, db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode))
	require.True(t, strings.EqualFold(journalMode, mode), "journal mode %s", mode)
	return db
}

// stuckBackup reports a BUSY-style retry on every step: not done, no error,
// and no pages copied.
type stuckBackup struct {
	steps, finishes int
}

func (b *stuckBackup) Step(int) (bool, error) {
	b.steps++
	return false, nil
}

func (b *stuckBackup) Remaining() int { return 512 }

func (b *stuckBackup) Finish() error {
	b.finishes++
	return nil
}

func TestCopySQLiteBackupStopsWhenStepsMakeNoProgress(t *testing.T) {
	backup := &stuckBackup{}

	err := copySQLiteBackup(t.Context(), backup)

	require.ErrorContains(t, err, "no progress after 1000 steps with 512 pages left")
	assert.Equal(t, 1001, backup.steps, "first step sets the baseline")
	assert.Equal(t, 1, backup.finishes)
}
