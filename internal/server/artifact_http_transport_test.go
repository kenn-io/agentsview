package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestArtifactHTTPTransportCanceledExchangeReleasesAllServerCursorsPromptly(t *testing.T) {
	const (
		peerOrigin = "peer-0000-a1b2c3"
		token      = "secret"
		deleteWait = 3 * time.Second
	)
	peer := httptest.NewUnstartedServer(nil)
	peerURL := "http://" + peer.Listener.Addr().String()
	te := setupArtifact(t,
		withAuth(token),
		withArtifactOrigin("zzserver-a1b2c3"),
		withPublicURL(peerURL),
	)
	for index := range 513 {
		origin := fmt.Sprintf("peer-%04d-a1b2c3", index)
		seedArtifactStore(t, te.artifactStore, origin, artifact.KindRaw,
			fmt.Appendf(nil, "origin-%04d", index))
	}
	for index := range 513 {
		seedArtifactStore(t, te.artifactStore, peerOrigin, artifact.KindRaw,
			fmt.Appendf(nil, "raw-%04d", index))
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var deletes, canceledDeletes atomic.Int32
	peer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/raw/") {
			cancel()
			<-r.Context().Done()
			return
		}
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/cursors/") {
			response := httptest.NewRecorder()
			te.srv.Handler().ServeHTTP(response, r)
			deletes.Add(1)
			select {
			case <-r.Context().Done():
				canceledDeletes.Add(1)
			case <-time.After(deleteWait):
			}
			for key, values := range response.Header() {
				w.Header()[key] = append([]string(nil), values...)
			}
			w.WriteHeader(response.Code)
			_, _ = io.Copy(w, response.Body)
			return
		}
		te.srv.Handler().ServeHTTP(w, r)
	})
	peer.Start()
	defer peer.Close()

	clientDB, clientDir := newClientNode(t, "sess-1", "alpha")
	_, err := artifact.Sync(ctx, clientDB, artifact.SyncOptions{
		DataDir: clientDir,
		Target:  peerURL,
		Origin:  "zzclient-a1b2c3",
		Token:   token,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(2), deletes.Load(), "origin and index cursors must both be released")
	assert.Equal(t, int32(2), canceledDeletes.Load(),
		"each cursor cleanup request must use its bounded context instead of the 120-second peer timeout")
}

func TestArtifactHTTPTransportPreExchangeFailuresReleasePreparedCursor(t *testing.T) {
	const (
		attempts = 3
		token    = "secret"
	)
	peer := httptest.NewUnstartedServer(nil)
	peerURL := "http://" + peer.Listener.Addr().String()
	te := setupArtifact(t, withAuth(token), withArtifactOrigin("server-a1b2c3"), withPublicURL(peerURL))
	for index := range 513 {
		origin := fmt.Sprintf("peer-%04d-a1b2c3", index)
		seedArtifactStore(t, te.artifactStore, origin, artifact.KindRaw,
			fmt.Appendf(nil, "origin-%04d", index))
	}

	var deletes atomic.Int32
	peer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/cursors/") {
			deletes.Add(1)
		}
		te.srv.Handler().ServeHTTP(w, r)
	})
	peer.Start()
	defer peer.Close()
	clientDB, clientDir := newClientNode(t, "sess-1", "alpha")

	for range attempts {
		_, err := artifact.Sync(t.Context(), clientDB, artifact.SyncOptions{
			DataDir: clientDir,
			Target:  peerURL,
			Origin:  "invalid/origin",
			Token:   token,
		})
		require.Error(t, err)
		assert.ErrorContains(t, err, "invalid artifact origin")
	}

	assert.Equal(t, int32(attempts), deletes.Load(),
		"each failed sync must release its prepared origin cursor")
}

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
	te := setupArtifact(t, withAuth(token), withArtifactOrigin("desktop-d4e5f6"))
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
	repository, err := artifact.OpenRepository(ctx, aDir)
	require.NoError(t, err)
	recorder := artifact.NewMetadataRecorder(aDB, artifact.MetadataRecorderOptions{
		Store:  repository.Content(),
		Origin: aOrigin,
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
	require.NoError(t, repository.Close())

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

	// Node B owns a session but has not run a separate artifact publisher.
	te := setupArtifact(t, withAuth(token), withArtifactOrigin(bOrigin))
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
	displayName := "Renamed before HTTP publishing"
	require.NoError(t, te.db.RenameSession("remote-1", &displayName))

	aDB, aDir := newClientNode(t, "sess-1", "alpha")
	syncResult, err := artifact.Sync(ctx, aDB, artifact.SyncOptions{
		DataDir: aDir,
		Target:  peer.URL,
		Origin:  "laptop-a1b2c3",
		Token:   token,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, syncResult.ImportedSessions)
	assert.Equal(t, 1, syncResult.ImportedMetadata)
	pendingImports, err := aDB.PendingArtifactImports(ctx, 1, 10)
	require.NoError(t, err)
	assert.Empty(t, pendingImports)

	// A pulled B's session.
	gotA, err := aDB.GetSession(ctx, bOrigin+"~remote-1")
	require.NoError(t, err)
	require.NotNil(t, gotA, "client should pull the remote session")
	assert.Equal(t, "bravo", gotA.Project)
	require.NotNil(t, gotA.DisplayName)
	assert.Equal(t, displayName, *gotA.DisplayName)
}

func TestArtifactHTTPTransportRejectsBadToken(t *testing.T) {
	te := setupArtifact(t, withAuth("secret"), withArtifactOrigin("desktop-d4e5f6"))
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
