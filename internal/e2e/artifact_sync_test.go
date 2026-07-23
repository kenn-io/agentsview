//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

const e2eToken = "artifact-e2e-token"

func TestArtifactSyncTwoInstanceFolderAndHTTP(t *testing.T) {
	ctx := context.Background()
	root := preservedWorkspace(t)
	share := filepath.Join(root, "share")
	require.NoError(t, os.MkdirAll(share, 0o755))

	a := openE2ENode(t, filepath.Join(root, "node-a"), "laptop-a1b2c3")
	defer a.Close()
	b := openE2ENode(t, filepath.Join(root, "node-b"), "desktop-d4e5f6")
	defer b.Close()

	seedE2ESession(t, a.DB, "sess-1", "alpha", "world")

	folderSync(t, a, share)
	folderSync(t, b, share)

	importedID := a.Origin + "~sess-1"
	assertSessionProject(t, b.DB, importedID, "alpha")
	assertSearchFinds(t, b.DB, "world", importedID)

	renameWithMetadata(t, a, "sess-1", "Alpha from laptop")
	folderSync(t, a, share)
	folderSync(t, b, share)
	assertSessionDisplayName(t, b.DB, importedID, "Alpha from laptop")

	renameWithMetadata(t, b, importedID, "Bravo from desktop")
	folderSync(t, b, share)
	folderSync(t, a, share)
	assertSessionDisplayName(t, a.DB, "sess-1", "Bravo from desktop")

	b.Close()
	b = openE2ENode(t, filepath.Join(root, "node-b"), "desktop-d4e5f6")
	defer b.Close()

	require.NoError(t, a.DB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "planet", ContentLength: 6},
	}))
	_, err := artifact.ExportToStore(ctx, a.DB, a.Repository.Content(), artifact.ExportOptions{
		Origin: a.Origin,
		Full:   true,
	})
	require.NoError(t, err)
	postOriginArtifacts(t, a, b, a.Origin)

	assertMessagesContain(t, b.DB, importedID, "planet")
	assertSearchFinds(t, b.DB, "planet", importedID)

	first := writeForeignRename(t, b, "writer-a1b2c3", importedID, "Fork one")
	second := writeForeignRename(t, b, "writer-b1b2c3", importedID, "Fork two")
	coordinator := artifact.NewStoreImportCoordinator(
		b.DB, b.Repository.Content(), b.Origin,
	)
	require.NoError(t, coordinator.RecordChanged(ctx, first))
	require.NoError(t, coordinator.RecordChanged(ctx, second))
	imported, err := coordinator.Finalize(ctx)
	require.NoError(t, err)
	assert.NotZero(t, imported.Metadata)

	conflicts, err := b.DB.ListMetadataConflicts(ctx, []string{importedID})
	require.NoError(t, err)
	require.NotEmpty(t, conflicts)
	assert.Equal(t, "display_name", conflicts[0].Field)

	apiConflicts := getMetadataConflicts(t, b, importedID)
	require.NotEmpty(t, apiConflicts.Conflicts)
	assert.Equal(t, importedID, apiConflicts.Conflicts[0].SessionGID)
}

type e2eNode struct {
	DataDir    string
	DBPath     string
	Origin     string
	DB         *db.DB
	Server     *httptest.Server
	App        *server.Server
	Repository *artifact.Repository
}

func openE2ENode(t *testing.T, dataDir, origin string) *e2eNode {
	t.Helper()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	dbPath := filepath.Join(dataDir, "sessions.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)

	emptyAgentDir := filepath.Join(dataDir, "empty-agent-dir")
	require.NoError(t, os.MkdirAll(emptyAgentDir, 0o755))
	broadcaster := server.NewBroadcaster(0)
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {emptyAgentDir},
		},
		Machine: origin,
		Emitter: broadcaster,
	})
	cfg := config.Config{
		Host:             "127.0.0.1",
		Port:             0,
		DataDir:          dataDir,
		DBPath:           dbPath,
		WriteTimeout:     30 * time.Second,
		RequireAuth:      true,
		AuthToken:        e2eToken,
		ArtifactOriginID: origin,
	}
	srv := server.New(cfg, database, engine,
		server.WithBroadcaster(broadcaster),
		server.WithArtifactStore(repository.Content()),
	)
	return &e2eNode{
		DataDir:    dataDir,
		DBPath:     dbPath,
		Origin:     origin,
		DB:         database,
		Server:     httptest.NewServer(srv.Handler()),
		App:        srv,
		Repository: repository,
	}
}

func (n *e2eNode) Close() {
	if n == nil {
		return
	}
	if n.Server != nil {
		n.Server.Close()
		n.Server = nil
	}
	if n.App != nil {
		_ = n.App.Shutdown(context.Background())
		n.App = nil
	}
	if n.Repository != nil {
		_ = n.Repository.Close()
		n.Repository = nil
	}
	if n.DB != nil {
		n.DB.Close()
		n.DB = nil
	}
}

func preservedWorkspace(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "agentsview-artifact-e2e-*")
	require.NoError(t, err)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("preserved artifact sync e2e workspace: %s", root)
			return
		}
		require.NoError(t, os.RemoveAll(root))
	})
	return root
}

func seedE2ESession(t *testing.T, database *db.DB, id, project, assistantText string) {
	t.Helper()
	started := "2026-06-14T01:02:03Z"
	ended := "2026-06-14T01:03:03Z"
	first := "hello"
	dbtest.SeedSession(t, database, id, project, func(s *db.Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.FirstMessage = &first
		s.StartedAt = &started
		s.EndedAt = &ended
	})
	require.NoError(t, database.ReplaceSessionMessages(id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: id, Ordinal: 1, Role: "assistant", Content: assistantText, ContentLength: len(assistantText)},
	}))
}

func folderSync(t *testing.T, n *e2eNode, share string) artifact.SyncResult {
	t.Helper()
	res, err := artifact.SyncWithRepository(context.Background(), n.DB, n.Repository, artifact.SyncOptions{
		DataDir: n.DataDir,
		Target:  share,
		Origin:  n.Origin,
	})
	require.NoError(t, err)
	return res
}

func renameWithMetadata(t *testing.T, n *e2eNode, sessionID, displayName string) {
	t.Helper()
	require.NoError(t, n.DB.RenameSession(sessionID, &displayName))
	// A local edit records its own replay register in this node's db, exactly as
	// the rename handler does.
	appendRenameArtifact(t, n.DB, n.Repository.Content(), n.Origin, sessionID, displayName)
}

func writeForeignRename(
	t *testing.T, n *e2eNode, origin, sessionID, displayName string,
) artifact.Entry {
	t.Helper()
	// A foreign origin's event arrives as an artifact file written by another
	// machine. Record it through a throwaway db so it is not pre-marked applied
	// in this node's db, leaving the real import to replay it.
	scratch, err := db.Open(filepath.Join(t.TempDir(), "scratch.db"))
	require.NoError(t, err)
	t.Cleanup(func() { scratch.Close() })
	return appendRenameArtifact(t, scratch, n.Repository.Content(), origin, sessionID, displayName)
}

func appendRenameArtifact(
	t *testing.T, database *db.DB, store artifact.ArtifactStore, origin, sessionID, displayName string,
) artifact.Entry {
	t.Helper()
	value, err := json.Marshal(struct {
		DisplayName *string `json:"display_name"`
	}{DisplayName: &displayName})
	require.NoError(t, err)
	recorder := artifact.NewMetadataRecorder(database, artifact.MetadataRecorderOptions{
		Store:  store,
		Origin: origin,
	})
	record, err := recorder.Append(context.Background(), artifact.MetadataEventInput{
		SessionID: sessionID,
		Op:        artifact.MetadataOpRename,
		Value:     json.RawMessage(value),
	})
	require.NoError(t, err)
	entry, err := store.Stat(t.Context(), record.Ref)
	require.NoError(t, err)
	return entry
}

func postOriginArtifacts(t *testing.T, from, to *e2eNode, origin string) {
	t.Helper()
	for _, kind := range []artifact.Kind{
		artifact.KindSegments,
		artifact.KindRaw,
		artifact.KindManifests,
		artifact.KindMeta,
		artifact.KindCheckpoints,
	} {
		iterator, err := from.Repository.Content().Entries(t.Context(), origin, kind)
		require.NoError(t, err)
		for {
			entries, nextErr := iterator.Next(t.Context(), 100)
			require.True(t, nextErr == nil || errors.Is(nextErr, io.EOF))
			for _, entry := range entries {
				_, reader, err := from.Repository.Content().Open(t.Context(), entry.Ref)
				require.NoError(t, err)
				wire, err := artifact.ToWireRef(entry.Ref)
				require.NoError(t, err)
				var body bytes.Buffer
				require.NoError(t, artifact.EncodeWire(t.Context(), entry.Ref, reader, &body))
				require.NoError(t, reader.Verify())
				require.NoError(t, reader.Close())
				postArtifact(t, to, wire, body.Bytes())
			}
			if errors.Is(nextErr, io.EOF) {
				break
			}
		}
		require.NoError(t, iterator.Close())
	}
}

func postArtifact(t *testing.T, to *e2eNode, wire artifact.WireRef, body []byte) {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodPost,
		to.Server.URL+"/api/v1/artifacts/"+wire.Origin+"/"+string(wire.Kind)+"/"+url.PathEscape(wire.Name),
		bytes.NewReader(body),
	)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+e2eToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

type metadataConflictsResponse struct {
	Conflicts []db.MetadataConflict `json:"conflicts"`
}

func getMetadataConflicts(t *testing.T, n *e2eNode, sessionID string) metadataConflictsResponse {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodGet,
		n.Server.URL+"/api/v1/sessions/"+url.PathEscape(sessionID)+"/metadata-conflicts",
		nil,
	)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+e2eToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out metadataConflictsResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func assertSessionProject(t *testing.T, database *db.DB, sessionID, project string) {
	t.Helper()
	sess, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, project, sess.Project)
}

func assertSessionDisplayName(t *testing.T, database *db.DB, sessionID, displayName string) {
	t.Helper()
	sess, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.NotNil(t, sess.DisplayName)
	assert.Equal(t, displayName, *sess.DisplayName)
}

func assertMessagesContain(t *testing.T, database *db.DB, sessionID, text string) {
	t.Helper()
	msgs, err := database.GetAllMessages(context.Background(), sessionID)
	require.NoError(t, err)
	for _, msg := range msgs {
		if msg.Content == text {
			return
		}
	}
	require.Fail(t, fmt.Sprintf("session %s messages did not contain %q", sessionID, text))
}

func assertSearchFinds(t *testing.T, database *db.DB, query, sessionID string) {
	t.Helper()
	page, err := database.Search(context.Background(), db.SearchFilter{
		Query: query,
		Limit: 10,
	})
	require.NoError(t, err)
	for _, result := range page.Results {
		if result.SessionID == sessionID {
			return
		}
	}
	require.Fail(t, fmt.Sprintf("search %q did not find %s", query, sessionID))
}
