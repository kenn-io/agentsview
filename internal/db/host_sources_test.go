package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClearHostSources(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	path := t.TempDir()
	require.NoError(t, d.SetSyncState(ctx, MachineLabelKeyPrefix+"machine-a", "Laptop"))
	require.NoError(t, d.SetSyncState(ctx, MachineAliasKeyPrefix+"old-machine", "machine-a"))
	require.NoError(t, d.SetSyncState(ctx, "machine_origin:machine-a", "origin"))
	require.NoError(t, d.SetSyncState(ctx, "replica-position", "123"))
	require.NoError(t, d.EnableArchiveOnly(ctx))
	require.NoError(t, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, project, cwd, file_path) VALUES('a', ?, ?, ?)`, path, path, path)
		return err
	}))
	require.NoError(t, d.ClearHostSources(ctx))
	var project, cwd string
	var filePath sql.NullString
	require.NoError(t, d.Reader().QueryRow(ctx, `SELECT project, cwd, file_path FROM sessions WHERE id='a'`).Scan(&project, &cwd, &filePath))
	assert.Equal(t, CopiedProjectLabel(path), project)
	assert.Empty(t, cwd)
	assert.False(t, filePath.Valid)
	detached, err := d.HostPathsDetached(ctx)
	require.NoError(t, err)
	assert.True(t, detached)
	labels, err := d.GetMachineLabels(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"machine-a": "Laptop"}, labels)
	aliases, err := d.GetMachineAliases(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"old-machine": "machine-a"}, aliases)
	origin, err := d.GetSyncState(ctx, "machine_origin:machine-a")
	require.NoError(t, err)
	assert.Equal(t, "origin", origin)
	require.ErrorIs(t, d.RequireSourceSync(ctx), ErrArchiveOnly)
	position, err := d.GetSyncState(ctx, "replica-position")
	require.NoError(t, err)
	assert.Empty(t, position)
}

func TestCopyProjectLabels(t *testing.T) {
	labels := []string{"/work/project", `C:\work\project`, `\\server\share\project`}
	seen := map[string]bool{}
	for _, label := range labels {
		alias := CopiedProjectLabel(label)
		assert.Regexp(t, `^copied-project-[0-9a-f]{64}$`, alias)
		assert.False(t, seen[alias])
		seen[alias] = true
	}
	assert.Equal(t, "project-a", CopiedProjectLabel("project-a"))
}
