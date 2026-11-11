//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCockroachSchemaAndCodexScope(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_cockroach_fresh_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(t.Context(), pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(t.Context(), pg, schema))

	_, err = pg.ExecContext(t.Context(), `DELETE FROM sync_metadata WHERE key = $1`, codexThreadScopeMetadataKey)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), `INSERT INTO excluded_sessions (id) VALUES ($1)`, codexTrashThread)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), `INSERT INTO sessions (id, machine, project, agent, deleted_at) VALUES ($1, 'machine', 'project', 'codex', NOW())`, codexTrashThread)
	require.NoError(t, err)
	require.NoError(t, backfillCodexThreadScopePG(t.Context(), pg))
	var excluded, trashed bool
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT include_codex_pages FROM excluded_sessions WHERE id = $1`, codexTrashThread).Scan(&excluded))
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT trash_includes_codex_pages FROM sessions WHERE id = $1`, codexTrashThread).Scan(&trashed))
	require.True(t, excluded)
	require.True(t, trashed)
}
