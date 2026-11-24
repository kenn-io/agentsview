package testutil

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// WriteCodexState creates a Codex thread index containing one rollout.
func WriteCodexState(t *testing.T, home, file, thread, page string) {
	t.Helper()
	state, err := sql.Open("sqlite3", filepath.Join(home, file))
	require.NoError(t, err)
	t.Cleanup(func() { _ = state.Close() })
	_, err = state.ExecContext(t.Context(), `CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = state.ExecContext(t.Context(), `INSERT INTO threads VALUES (?, ?)`, thread, "rollout-2026-01-01-"+page+".jsonl")
	require.NoError(t, err)
	require.NoError(t, state.Close())
}
