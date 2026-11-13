package main

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestExportSchemaUpgradeRefusesWhileOwnerHeld(t *testing.T) {
	dir, cfg := writeDBConfigForTest(t)
	seed := dbtest.OpenTestDBAt(t, cfg.DBPath)
	require.NoError(t, seed.Close())
	owner := holdWriteOwnerLockForTest(t, dir)

	current, err := openExportReadOnlyDB(t.Context(), cfg)
	if current != nil {
		t.Cleanup(func() { _ = current.Close() })
	}
	require.NoError(t, err)
	require.NoError(t, current.Close())

	raw, err := sql.Open("sqlite3", cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	_, err = raw.ExecContext(t.Context(), `DROP TABLE session_project_identity_snapshots`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	before, err := os.ReadFile(cfg.DBPath)
	require.NoError(t, err)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Stat(cfg.DBPath + suffix)
		require.ErrorIs(t, err, os.ErrNotExist)
	}

	blocked, err := openExportReadOnlyDB(t.Context(), cfg)
	if blocked != nil {
		t.Cleanup(func() { _ = blocked.Close() })
	}
	require.ErrorContains(t, err, writeOwnerLockPath(dir))
	assert.Nil(t, blocked)
	after, err := os.ReadFile(cfg.DBPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	_, err = os.Stat(cfg.DBPath + "-journal")
	assert.ErrorIs(t, err, os.ErrNotExist)
	wal, err := os.Stat(cfg.DBPath + "-wal")
	if err == nil {
		assert.Zero(t, wal.Size())
	} else {
		assert.ErrorIs(t, err, os.ErrNotExist)
	}

	require.NoError(t, owner.Release())
	upgraded, err := openExportReadOnlyDB(t.Context(), cfg)
	if upgraded != nil {
		t.Cleanup(func() { _ = upgraded.Close() })
	}
	require.NoError(t, err)
	next := acquireWriteOwnerLockForTest(t, dir)
	require.NoError(t, next.Close())
	require.NoError(t, upgraded.Close())
}
