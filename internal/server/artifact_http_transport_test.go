package server_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

// newClientNode opens a client-only artifact store (a db plus data dir) that
// drives artifact.Sync against an HTTP peer.
func newClientNode(t *testing.T, sessionID, project string) (*db.DB, string) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "client.db"))
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	dbtest.SeedSession(t, database, sessionID, project, func(s *db.Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	require.NoError(t, database.ReplaceSessionMessages(sessionID, []db.Message{
		{SessionID: sessionID, Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: sessionID, Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5},
	}))
	return database, dir
}

func TestArtifactHTTPTransportSyncsSessionsAndMetadata(t *testing.T) {
	ctx := context.Background()
	const token = "secret"
	const aOrigin = "laptop-a1b2c3"

	// Node B: a real server exposing the artifact peer API behind auth.
	te := setup(t, withAuth(token), withArtifactOrigin("desktop-d4e5f6"))
	peer := httptest.NewServer(te.srv.Handler())
	defer peer.Close()

	aDB, aDir := newClientNode(t, "sess-1", "alpha")

	syncToPeer := func() {
		_, err := artifact.Sync(ctx, aDB, artifact.SyncOptions{
			DataDir: aDir,
			Target:  peer.URL,
			Origin:  aOrigin,
			Token:   token,
		})
		require.NoError(t, err)
	}

	// A pushes its session over HTTP; B imports it on receipt.
	syncToPeer()
	importedID := aOrigin + "~sess-1"
	gotB, err := te.db.GetSession(ctx, importedID)
	require.NoError(t, err)
	require.NotNil(t, gotB, "peer should import the pushed session")
	assert.Equal(t, "alpha", gotB.Project)

	// A renames the session and syncs again; the metadata event is enumerated
	// via the index route, posted, and replayed on B.
	display := "Renamed on A"
	require.NoError(t, aDB.RenameSession("sess-1", &display))
	recorder := artifact.NewMetadataRecorder(aDB, artifact.MetadataRecorderOptions{
		DataDir: aDir,
		Origin:  aOrigin,
	})
	value, err := json.Marshal(struct {
		DisplayName *string `json:"display_name"`
	}{DisplayName: &display})
	require.NoError(t, err)
	_, err = recorder.Append(ctx, artifact.MetadataEventInput{
		SessionID: "sess-1",
		Op:        artifact.MetadataOpRename,
		Value:     value,
	})
	require.NoError(t, err)

	syncToPeer()
	gotB, err = te.db.GetSession(ctx, importedID)
	require.NoError(t, err)
	require.NotNil(t, gotB)
	require.NotNil(t, gotB.DisplayName)
	assert.Equal(t, display, *gotB.DisplayName)
}

func TestArtifactHTTPTransportPullsRemoteSessions(t *testing.T) {
	ctx := context.Background()
	const token = "secret"
	const bOrigin = "desktop-d4e5f6"

	// Node B owns a session and exports it to its local artifact store.
	te := setup(t, withAuth(token), withArtifactOrigin(bOrigin))
	peer := httptest.NewServer(te.srv.Handler())
	defer peer.Close()
	dbtest.SeedSession(t, te.db, "remote-1", "bravo", func(s *db.Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	require.NoError(t, te.db.ReplaceSessionMessages("remote-1", []db.Message{
		{SessionID: "remote-1", Ordinal: 0, Role: "user", Content: "ping", ContentLength: 4},
		{SessionID: "remote-1", Ordinal: 1, Role: "assistant", Content: "pong", ContentLength: 4},
	}))
	_, err := artifact.Export(ctx, te.db, filepath.Join(te.dataDir, "artifacts"), bOrigin)
	require.NoError(t, err)

	aDB, aDir := newClientNode(t, "sess-1", "alpha")
	_, err = artifact.Sync(ctx, aDB, artifact.SyncOptions{
		DataDir: aDir,
		Target:  peer.URL,
		Origin:  "laptop-a1b2c3",
		Token:   token,
	})
	require.NoError(t, err)

	// A pulled B's session.
	gotA, err := aDB.GetSession(ctx, bOrigin+"~remote-1")
	require.NoError(t, err)
	require.NotNil(t, gotA, "client should pull the remote session")
	assert.Equal(t, "bravo", gotA.Project)
}

func TestArtifactHTTPTransportRejectsBadToken(t *testing.T) {
	te := setup(t, withAuth("secret"), withArtifactOrigin("desktop-d4e5f6"))
	peer := httptest.NewServer(te.srv.Handler())
	defer peer.Close()
	aDB, aDir := newClientNode(t, "sess-1", "alpha")

	_, err := artifact.Sync(context.Background(), aDB, artifact.SyncOptions{
		DataDir: aDir,
		Target:  peer.URL,
		Origin:  "laptop-a1b2c3",
		Token:   "wrong",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "peer")
}
