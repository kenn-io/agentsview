package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

type artifactOriginsBody struct {
	Origins []string `json:"origins"`
}

type artifactPostBody struct {
	Origin    string `json:"origin"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Hash      string `json:"hash,omitempty"`
	Size      int64  `json:"size"`
	Duplicate bool   `json:"duplicate"`
}

func TestArtifactPeerRoutesRequireBearerAuthWhenConfigured(t *testing.T) {
	te := setup(t, withAuth("secret"))

	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/origins", nil, "")
	assertStatus(t, w, http.StatusUnauthorized)

	w = artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/origins", nil, "secret")
	assertStatus(t, w, http.StatusOK)
}

func TestArtifactPeerRoutesPostDuplicateAndFetch(t *testing.T) {
	te := setup(t, withAuth("secret"))
	origin := "peer-a1b2c3"
	metadataBody, metadataName := peerMetadataArtifact(
		origin,
		"2026-06-14T010203.000000001Z-peer-a1b2c3",
	)

	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		metadataBody, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	posted := decode[artifactPostBody](t, w)
	assert.False(t, posted.Duplicate)
	assert.Equal(t, "meta", posted.Kind)
	assert.Equal(t, metadataName, posted.Name)

	w = artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		metadataBody, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	posted = decode[artifactPostBody](t, w)
	assert.True(t, posted.Duplicate)

	w = artifactPeerRequest(
		t, te, http.MethodGet,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		nil, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, metadataBody, w.Body.Bytes())

	checkpoint := []byte(`{"origin":"peer-a1b2c3","seq":1,"sessions":{},"v":1}` + "\n")
	w = artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/checkpoints/cp-0000000001",
		checkpoint, "secret",
	)
	assertStatus(t, w, http.StatusOK)

	w = artifactPeerRequest(
		t, te, http.MethodGet,
		"/api/v1/artifacts/"+origin+"/checkpoint",
		nil, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, checkpoint, w.Body.Bytes())

	w = artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/origins", nil, "secret")
	assertStatus(t, w, http.StatusOK)
	origins := decode[artifactOriginsBody](t, w)
	assert.Contains(t, origins.Origins, origin)
}

type artifactPeerBody struct {
	Origin            string `json:"origin"`
	IsLocal           bool   `json:"is_local"`
	CheckpointSeq     int    `json:"checkpoint_seq"`
	PublishedSessions int    `json:"published_sessions"`
	LocalSessions     int    `json:"local_sessions"`
	LastPublished     string `json:"last_published"`
}

type artifactPeersBody struct {
	LocalOrigin   string             `json:"local_origin"`
	Peers         []artifactPeerBody `json:"peers"`
	ConflictCount int                `json:"conflict_count"`
}

func TestArtifactPeersStatus(t *testing.T) {
	local := "desktop-d4e5f6"
	te := setup(t, withArtifactOrigin(local))
	ctx := context.Background()
	artifactRoot := filepath.Join(te.dataDir, "artifacts")
	first := "hi"

	// Two owned sessions, exported so the local origin gets a checkpoint.
	dbtest.SeedSession(t, te.db, "local-1", "proj", func(s *db.Session) { s.FirstMessage = &first })
	dbtest.SeedSession(t, te.db, "local-2", "proj", func(s *db.Session) { s.FirstMessage = &first })
	exported, err := artifact.Export(ctx, te.db, artifactRoot, local)
	require.NoError(t, err)
	require.Equal(t, 2, exported)

	// A foreign peer publishes one session that the server imports.
	origin := "peer-a1b2c3"
	peerRoot := t.TempDir()
	peerDB, err := db.Open(filepath.Join(t.TempDir(), "peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { peerDB.Close() })
	dbtest.SeedSession(t, peerDB, "sess-1", "alpha", func(s *db.Session) { s.FirstMessage = &first })
	require.NoError(t, peerDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
	}))
	_, err = artifact.Export(ctx, peerDB, peerRoot, origin)
	require.NoError(t, err)
	postArtifactFile(t, te, origin, "segments", oneArtifactPath(t, peerRoot, origin, "segments", "*"))
	postArtifactFile(t, te, origin, "manifests", oneArtifactPath(t, peerRoot, origin, "manifests", "*"))
	postArtifactFile(t, te, origin, "checkpoints", oneArtifactPath(t, peerRoot, origin, "checkpoints", "*"))

	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/peers", nil, "")
	assertStatus(t, w, http.StatusOK)
	body := decode[artifactPeersBody](t, w)

	assert.Equal(t, local, body.LocalOrigin)
	assert.Equal(t, 0, body.ConflictCount)
	require.Len(t, body.Peers, 2)

	byOrigin := map[string]artifactPeerBody{}
	for _, p := range body.Peers {
		byOrigin[p.Origin] = p
	}

	localPeer, ok := byOrigin[local]
	require.True(t, ok, "local origin present in peers")
	assert.True(t, localPeer.IsLocal)
	assert.Equal(t, 2, localPeer.PublishedSessions)
	assert.Equal(t, 2, localPeer.LocalSessions)
	assert.NotEmpty(t, localPeer.LastPublished)

	peer, ok := byOrigin[origin]
	require.True(t, ok, "foreign origin present in peers")
	assert.False(t, peer.IsLocal)
	assert.Equal(t, 1, peer.PublishedSessions)
	assert.Equal(t, 1, peer.LocalSessions)
	assert.Equal(t, 1, peer.CheckpointSeq)
}

func TestArtifactPeersStatusEmptyWithoutDataDir(t *testing.T) {
	te := setup(t, withArtifactOrigin("desktop-d4e5f6"))
	// No artifacts exported yet: the local machine still appears, with no
	// published sessions and no checkpoint.
	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/peers", nil, "")
	assertStatus(t, w, http.StatusOK)
	body := decode[artifactPeersBody](t, w)
	assert.Equal(t, "desktop-d4e5f6", body.LocalOrigin)
	require.Len(t, body.Peers, 1)
	assert.True(t, body.Peers[0].IsLocal)
	assert.Equal(t, 0, body.Peers[0].PublishedSessions)
	assert.Empty(t, body.Peers[0].LastPublished)
}

func TestArtifactPeerPostRejectsHashMismatch(t *testing.T) {
	te := setup(t, withAuth("secret"))
	origin := "peer-a1b2c3"
	metadataBody, _ := peerMetadataArtifact(
		origin,
		"2026-06-14T010203.000000001Z-peer-a1b2c3",
	)
	badName := "2026-06-14T010203.000000001Z-peer-a1b2c3-" + strings.Repeat("0", 64)

	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(badName),
		metadataBody, "secret",
	)
	assertStatus(t, w, http.StatusBadRequest)
}

func TestArtifactPeerPostImportsAndEmitsDataChanged(t *testing.T) {
	te := setup(t, withArtifactOrigin("desktop-d4e5f6"))
	origin := "peer-a1b2c3"
	artifactRoot := t.TempDir()
	peerDB, err := db.Open(filepath.Join(t.TempDir(), "peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { peerDB.Close() })

	first := "hello"
	started := "2026-06-14T01:02:03Z"
	ended := "2026-06-14T01:03:03Z"
	dbtest.SeedSession(t, peerDB, "sess-1", "alpha", func(s *db.Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.FirstMessage = &first
		s.StartedAt = &started
		s.EndedAt = &ended
	})
	require.NoError(t, peerDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5},
	}))
	_, err = artifact.Export(context.Background(), peerDB, artifactRoot, origin)
	require.NoError(t, err)

	postArtifactFile(t, te, origin, "segments",
		oneArtifactPath(t, artifactRoot, origin, "segments", "*"))
	postArtifactFile(t, te, origin, "manifests",
		oneArtifactPath(t, artifactRoot, origin, "manifests", "*"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	stream := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		te.handler.ServeHTTP(stream, req)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)

	postArtifactFile(t, te, origin, "checkpoints",
		oneArtifactPath(t, artifactRoot, origin, "checkpoints", "*"))
	te.waitForSSEEvent(t, stream, "data_changed", 3*time.Second)

	got, err := te.db.GetSession(context.Background(), origin+"~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, origin, got.Machine)
	assert.Equal(t, "alpha", got.Project)

	cancel()
	<-done
}

func artifactPeerRequest(
	t *testing.T,
	te *testEnv,
	method string,
	path string,
	body []byte,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	return w
}

func postArtifactFile(
	t *testing.T,
	te *testEnv,
	origin string,
	kind string,
	path string,
) {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/"+kind+"/"+url.PathEscape(filepath.Base(path)),
		body, "",
	)
	assertStatus(t, w, http.StatusOK)
}

func oneArtifactPath(
	t *testing.T,
	root string,
	origin string,
	kind string,
	pattern string,
) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, origin, kind, pattern))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	return paths[0]
}

func peerMetadataArtifact(origin, hlc string) ([]byte, string) {
	body := []byte(`{"hlc":"` + hlc + `","op":"rename","origin":"` + origin + `","session_gid":"` + origin + `~sess-1","v":1,"value":{"display_name":"Remote"}}` + "\n")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	return body, hlc + "-" + hash + ".json"
}
