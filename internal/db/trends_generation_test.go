package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrendsGenerationMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := Open(t.Context(), path)
	require.NoError(t, err)
	insertSession(t, d, "retained", "project")
	require.NoError(t, d.Close())
	legacy, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = legacy.ExecContext(t.Context(), `DROP TRIGGER sessions_trends_generation_insert; DROP TRIGGER sessions_trends_generation_update; ALTER TABLE sessions DROP COLUMN trends_generation`)
	require.NoError(t, err)
	require.NoError(t, legacy.Close())
	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	var before string
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT trends_generation FROM sessions WHERE id='retained'`).Scan(&before))
	require.NotEmpty(t, before)
	require.NoError(t, d.Close())
	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	defer d.Close()
	var after string
	require.NoError(t, d.getReader().QueryRowContext(t.Context(), `SELECT trends_generation FROM sessions WHERE id='retained'`).Scan(&after))
	require.Equal(t, before, after)
}
