package testutil

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
)

// WriteCodexState creates a Codex thread index containing one rollout.
func WriteCodexState(t *testing.T, home, file, thread, page string) {
	t.Helper()
	tmp, err := os.CreateTemp(home, file+".*.tmp")
	require.NoError(t, err)
	tmpPath := tmp.Name()
	require.NoError(t, tmp.Close())
	t.Cleanup(func() { _ = os.Remove(tmpPath) })

	state, err := sql.Open("sqlite3", tmpPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })
	_, err = state.ExecContext(t.Context(), `CREATE TABLE _sqlx_migrations (version BIGINT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = state.ExecContext(t.Context(), `CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = state.ExecContext(t.Context(), `INSERT INTO _sqlx_migrations VALUES (54)`)
	require.NoError(t, err)
	_, err = state.ExecContext(t.Context(), `INSERT INTO threads VALUES (?, ?)`, thread, "rollout-2026-01-01-"+page+".jsonl")
	require.NoError(t, err)
	require.NoError(t, state.Close())
	require.NoError(t, atomicfile.Replace(tmpPath, filepath.Join(home, file)))
}
