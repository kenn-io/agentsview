package rawcapture

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
)

func TestSQLiteSnapshotKeepsInitialReadView(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	writer, err := sql.Open(sqliteSnapshotDriverName, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	_, err = writer.ExecContext(t.Context(), `PRAGMA journal_mode=WAL;
CREATE TABLE items (value TEXT NOT NULL);
INSERT INTO items VALUES ('captured');`)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	source, err := openSQLiteSnapshotSource(t.Context(), path, info)
	require.NoError(t, err)
	defer func() { require.NoError(t, source.Close()) }()

	// A writer can keep committing while capture holds its original read view.
	_, err = writer.ExecContext(t.Context(), `INSERT INTO items VALUES ('later')`)
	require.NoError(t, err)
	destination := filepath.Join(t.TempDir(), "snapshot.db")
	require.NoError(t, sqliteOnlineBackup(t.Context(), source.connection, destination, 1<<20))
	snapshot, err := sql.Open(sqliteSnapshotDriverName, sqliteSnapshotDSN(destination, true))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, snapshot.Close()) })
	var values string
	require.NoError(t, snapshot.QueryRowContext(t.Context(), `SELECT group_concat(value, ',') FROM items`).Scan(&values))
	assert.Equal(t, "captured", values)
}

func TestRunSQLiteBackupRejectsGrowthPastLimit(t *testing.T) {
	pageCount := 1
	steps := 0

	err := runSQLiteBackup(
		t.Context(), 4096, 4096,
		func() (bool, error) {
			steps++
			pageCount = 2
			return false, nil
		},
		func() int { return 1 },
		func() int { return pageCount },
	)

	require.ErrorIs(t, err, rawcheckpoint.ErrOutboxFull)
	assert.Equal(t, 1, steps)
}

func TestSnapshotSQLitePlanAddsBackupDeadline(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	capturer := New(store)
	backupErr := errors.New("stop after checking deadline")
	capturer.sqliteBackup = func(
		ctx context.Context, _ *sql.Conn, _ string, _ int64,
	) error {
		_, bounded := ctx.Deadline()
		assert.True(t, bounded)
		return backupErr
	}

	_, _, err := capturer.snapshotSQLitePlan(
		t.Context(),
		parser.RawCapturePlan{
			SourceKey: "source.db",
			Entries:   []parser.RawCaptureEntry{{Path: "source.db"}},
		},
		nil,
		4096,
	)

	require.ErrorIs(t, err, backupErr)
}
