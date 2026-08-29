package rawcapture

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
)

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
