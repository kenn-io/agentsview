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

// TestPGTranscriptFidelityRoundTripsAndRepushes verifies that the
// transcript_fidelity column is pushed to PostgreSQL, read back correctly,
// and that a change triggers a re-push via the IS DISTINCT FROM guard.
func TestPGTranscriptFidelityRoundTripsAndRepushes(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_transcript_fidelity_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}

	const sessID = "antigravity-cli:pg-fidelity-001"
	sess := db.Session{
		ID:                 sessID,
		Project:            "test-proj",
		Machine:            "test-machine",
		Agent:              "antigravity-cli",
		TranscriptFidelity: "summary",
		CreatedAt:          "2026-01-01T00:00:00Z",
	}
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession (summary)")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push (summary)")

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	got, err := store.GetSession(ctx, sessID)
	require.NoError(t, err, "GetSession after first push")
	require.NotNil(t, got, "session not found after first push")
	assert.Equal(t, "summary", got.TranscriptFidelity,
		"TranscriptFidelity should be 'summary' after first push")

	// Change fidelity and re-push — exercises the IS DISTINCT FROM clause.
	sess.TranscriptFidelity = "full"
	require.NoError(t, localDB.UpsertSession(sess), "UpsertSession (full)")

	// Clear the push watermark so the engine re-evaluates all sessions.
	require.NoError(t, localDB.SetSyncState("last_push_at", ""),
		"clearing last_push_at")
	require.NoError(t, localDB.SetSyncState(lastPushBoundaryStateKey, ""),
		"clearing boundary state")

	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push (full)")

	got, err = store.GetSession(ctx, sessID)
	require.NoError(t, err, "GetSession after second push")
	require.NotNil(t, got, "session not found after second push")
	assert.Equal(t, "full", got.TranscriptFidelity,
		"IS DISTINCT FROM must re-push the change to 'full'")
}
