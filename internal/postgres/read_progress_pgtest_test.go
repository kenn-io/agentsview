//go:build pgtest

package postgres

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestPushSessionTranscriptRevisionRoundTrip(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_read_progress_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, pg, schema))

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	defer localDB.Close()
	syncer := &Sync{
		pg: pg, local: localDB, machine: "test-machine",
		schema: schema, schemaDone: true,
	}
	hash := "transcript-hash"
	modified := "2026-07-12T12:00:00Z"
	sess := db.Session{
		ID: "read-progress", Project: "project", Machine: "test-machine",
		Agent: "claude", MessageCount: 1, UserMessageCount: 1,
		CreatedAt: "2026-07-12T12:00:00Z", FileHash: &hash,
		LocalModifiedAt: &modified,
	}
	markerID, err := syncer.pushMarkerID()
	require.NoError(t, err)
	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, syncer.pushSession(ctx, tx, sess, markerID, nil))
	require.NoError(t, tx.Commit())

	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err)
	defer store.Close()
	detail, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, detail)
	assertPGJSONTranscriptRevision(t, detail, hash)

	index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Limit: 50})
	require.NoError(t, err)
	require.Len(t, index.Sessions, 1)
	assertPGJSONTranscriptRevision(t, index.Sessions[0], hash)
}

func assertPGJSONTranscriptRevision(t *testing.T, value any, want string) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	assert.Equal(t, want, fields["transcript_revision"])
	assert.NotContains(t, fields, "file_hash")
	assert.NotContains(t, fields, "local_modified_at")
}
