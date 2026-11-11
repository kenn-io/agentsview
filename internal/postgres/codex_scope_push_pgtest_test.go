//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPushSessionWithCodexScope(t *testing.T) {
	const schema = "agentsview_codex_scope_push_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(t.Context(), pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	ctx := t.Context()
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	local := testDB(t)
	syncer := &Sync{pg: pg, local: local, machine: "workstation", schema: schema, schemaDone: true}
	seedProvenanceSession(t, local, "session-1", "workstation", "project", "")
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Equal(t, 1, result.SessionsPushed)
	var id string
	require.NoError(t, pg.QueryRowContext(ctx, `SELECT id FROM sessions WHERE id = $1`, "session-1").Scan(&id))
	require.Equal(t, "session-1", id)
}
