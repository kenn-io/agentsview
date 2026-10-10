//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestCodexLegacyExclusionsSurvivePageSplit(t *testing.T) {
	ctx := t.Context()
	const schema = "agentsview_codex_exclusion_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	// Start with the shipped exclusion table, before per-file Codex identities.
	_, err = pg.ExecContext(ctx, `CREATE SCHEMA agentsview_codex_exclusion_test;
		CREATE TABLE excluded_sessions (id TEXT PRIMARY KEY, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`)
	require.NoError(t, err)
	const threadUUID = "11111111-1111-4111-8111-111111111111"
	const pageSuffix = "_22222222-2222-4222-8222-222222222222"
	cases := []struct {
		id    string
		agent string
		scope bool
	}{
		{"codex:" + threadUUID, "codex", true},
		{"host-a~traex:" + threadUUID, "traex", true},
		{"augure-code:" + threadUUID, "augure-code", true},
		{"claude:" + threadUUID, "claude", false},
		{"codex:" + threadUUID + pageSuffix, "codex", false},
		{"raw:" + threadUUID, "codex", false},
	}
	for _, tc := range cases {
		_, err = pg.ExecContext(ctx, `INSERT INTO excluded_sessions(id) VALUES($1)`, tc.id)
		require.NoError(t, err)
	}
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	local := testDB(t)
	syncer := &Sync{pg: pg, local: local, machine: "machine", schema: schema, schemaDone: true}
	store := &Store{pg: pg}
	for _, tc := range cases {
		var scope bool
		require.NoError(t, pg.QueryRowContext(ctx, `SELECT include_codex_pages FROM excluded_sessions WHERE id=$1`, tc.id).Scan(&scope))
		assert.Equal(t, tc.scope, scope, tc.id)
		if tc.scope {
			id := tc.id + "_33333333-3333-4333-8333-333333333333"
			require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: tc.agent, Project: "sample", Machine: "machine"}))
		}
	}
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	for _, tc := range cases {
		if !tc.scope {
			continue
		}
		page, err := store.GetSessionFull(ctx, tc.id+"_33333333-3333-4333-8333-333333333333")
		require.NoError(t, err)
		assert.Nil(t, page, "an old permanent deletion must exclude the split page")
	}

	// A deletion after the migration keeps its per-file meaning through both
	// schema setup and the fast repair path used by later pushes.
	const newThread = "codex:44444444-4444-4444-8444-444444444444"
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: newThread, Agent: "codex", Project: "sample", Machine: "machine"}))
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.NoError(t, store.SoftDeleteSession(ctx, newThread))
	deleted, err := store.DeleteSessionIfTrashed(ctx, newThread)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	require.NoError(t, runSchemaDataRepairsPG(ctx, pg))
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: newThread + pageSuffix, Agent: "codex", Project: "sample", Machine: "machine"}))
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	page, err := store.GetSessionFull(ctx, newThread+pageSuffix)
	require.NoError(t, err)
	assert.NotNil(t, page, "new per-file deletion must leave sibling pages visible")
}
