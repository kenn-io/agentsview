//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPushSessionUpdateInvalidatesVectorPushState pins the staleness
// contract behind semantic readiness: when a session push materially
// changes a vector-relevant session field (transcript revision, deletion,
// automation, project, message counts), that session's vector_push_state
// rows are removed for every generation, because their doc_agg_hash values
// describe the previous transcript. Readiness must then count the session
// as missing until the vector phase re-pushes it. Unrelated metadata
// updates — a display-name rename, say — keep the recorded coverage.
func TestPushSessionUpdateInvalidatesVectorPushState(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_vector_invalidation_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}
	markerID, err := sync.pushMarkerID(ctx)
	require.NoError(t, err, "pushMarkerID")

	sess := db.Session{
		ID:           "vec-invalid-001",
		Project:      "test-proj",
		Machine:      "test-machine",
		Agent:        "claude",
		MessageCount: 1,
		CreatedAt:    "2026-06-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(ctx, sess), "UpsertSession")

	pushSessionTX := func() {
		tx, txErr := pg.BeginTx(ctx, nil)
		require.NoError(t, txErr, "BeginTx")
		require.NoError(t, sync.pushSession(ctx, tx, sess, markerID, nil), "pushSession")
		require.NoError(t, tx.Commit(), "Commit")
	}
	pushStateRows := func() int64 {
		var n int64
		require.NoError(t, pg.QueryRowContext(ctx,
			`SELECT count(*) FROM vector_push_state WHERE session_id = $1`,
			sess.ID).Scan(&n), "count push state")
		return n
	}
	seedPushState := func() {
		_, execErr := pg.ExecContext(ctx, `
			INSERT INTO vector_push_state (generation_id, session_id, doc_agg_hash)
			VALUES (1, $1, 'stale-hash')`, sess.ID)
		require.NoError(t, execErr, "seed push state")
	}

	// A fresh insert carries no vector coverage and removes none.
	pushSessionTX()
	assert.Equal(t, int64(0), pushStateRows(), "fresh insert has no push state")

	// A prior vector push records coverage for the session.
	seedPushState()

	// Re-pushing an unchanged session must keep the recorded coverage: the
	// guarded upsert changes no rows.
	pushSessionTX()
	assert.Equal(t, int64(1), pushStateRows(),
		"an unchanged session keeps its vector coverage")

	// A metadata-only rename is a material row change but not a
	// vector-relevant one: the recorded coverage survives.
	pDisplayName := "renamed session"
	sess.DisplayName = &pDisplayName
	require.NoError(t, localDB.UpsertSession(ctx, sess), "UpsertSession rename")
	pushSessionTX()
	assert.Equal(t, int64(1), pushStateRows(),
		"metadata-only updates preserve vector coverage")

	// A vector-relevant change (message count) invalidates the stale
	// coverage.
	sess.MessageCount = 2
	require.NoError(t, localDB.UpsertSession(ctx, sess), "UpsertSession update")
	pushSessionTX()
	assert.Equal(t, int64(0), pushStateRows(),
		"a vector-relevant change must drop the stale vector coverage")
}
