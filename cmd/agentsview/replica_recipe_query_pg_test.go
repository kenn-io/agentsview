//go:build pgtest

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
)

// A workstation with [vector] pushes and publishes its recipe; a container
// with only AGENTSVIEW_EMBEDDINGS_* adopts it and answers a semantic query
// from the real pgvector chunk table.
func TestReplicaRecipeQueryWithoutConfig(t *testing.T) {
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL not set; skipping PG tests")
	}
	const schema = "agentsview_recipe_query_test"
	admin, err := postgres.Open(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		_ = admin.Close()
	})
	_, err = admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err)

	type call struct{ auth, model, input string }
	var mu stdsync.Mutex
	var calls []call
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		raw, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) || !assert.NoError(t, json.Unmarshal(raw, &body)) {
			return
		}
		data := make([]map[string]any, len(body.Input))
		for i, input := range body.Input {
			mu.Lock()
			calls = append(calls, call{r.Header.Get("Authorization"), body.Model, input})
			mu.Unlock()
			vec := []float32{0, 1, 0, 0}
			if strings.Contains(input, "nearest") || strings.Contains(input, "find") {
				vec = []float32{1, 0, 0, 0}
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(endpoint.Close)

	// Workstation: build with [vector] and push to PostgreSQL.
	workstation := vectorTestConfig(t.TempDir())
	workstation.Vector.Embeddings = publishedRecipeConfig("published-model")
	workstation.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{
		"local": {Endpoint: endpoint.URL + "/v1", BatchSize: 8, Concurrency: 1, Timeout: "10s", MaxRetries: 1},
	}
	local := dbtest.OpenTestDBAt(t, workstation.DBPath)
	for id, content := range map[string]string{
		"session-one": "nearest literal content",
		"session-two": "far content",
	} {
		dbtest.SeedSessionWithMessages(t, local, id, "project",
			[]db.Message{dbtest.UserMsg(id, 0, content)},
			func(s *db.Session) { s.EndedAt = new("2026-01-01T00:00:00Z") })
	}
	require.NoError(t, runEmbeddingsBuildDirect(t.Context(), io.Discard, workstation,
		vector.BuildRequest{}))
	source := newVectorPushSource(workstation)
	t.Cleanup(func() { closeVectorPushSource(source) })
	sync, err := postgres.New(pgURL, schema, local, "workstation", true,
		storage.PusherOptions{VectorSource: source})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sync.Close() })
	require.NoError(t, sync.EnsureSchema(t.Context()))
	res, err := sync.PushWithOptions(t.Context(), storage.PushOptions{Full: true}, nil)
	require.NoError(t, err)
	if res.Vectors.Skipped {
		t.Skip(res.Vectors.SkippedReason)
	}
	require.Positive(t, res.Vectors.ChunksPushed)

	// A full embedded push repairs missing chunks even when document hashes match.
	chunkTable := fmt.Sprintf("%s.vector_chunks_g%d", schema, res.Vectors.GenerationID)
	var chunksBefore int
	require.NoError(t, admin.QueryRow("SELECT COUNT(*) FROM "+chunkTable).Scan(&chunksBefore))
	require.Positive(t, chunksBefore)
	_, err = admin.Exec("DELETE FROM " + chunkTable + " WHERE (doc_key, chunk_index) IN (SELECT doc_key, chunk_index FROM " + chunkTable + " LIMIT 1)")
	require.NoError(t, err)
	var chunksAfter int
	require.NoError(t, admin.QueryRow("SELECT COUNT(*) FROM "+chunkTable).Scan(&chunksAfter))
	require.Equal(t, chunksBefore-1, chunksAfter)
	backend := &localArchiveWriteBackend{
		appCfg: workstation, database: local,
		ensurePricing: func(context.Context, *db.DB) error { return nil },
	}
	target := storage.ConfiguredReplica{Target: storage.ReplicaTarget{
		URL: pgURL, Schema: schema, MachineName: "workstation", AllowInsecure: true, PushVectors: true,
	}}
	var repaired storage.PushResult
	captureStdout(t, func() {
		repaired, err = backend.ReplicaPush(t.Context(), pgReplica{}, target, ReplicaPushConfig{Full: true, Embed: true}, nil, nil)
	})
	require.NoError(t, err)
	assert.Equal(t, 2, repaired.SessionsPushed)
	assert.Equal(t, 2, repaired.Vectors.SessionsPushed)
	assert.Equal(t, 2, repaired.Vectors.DocsPushed)
	assert.Equal(t, chunksBefore, repaired.Vectors.ChunksPushed)
	assert.Zero(t, repaired.Vectors.SessionsUnchanged)
	require.NoError(t, admin.QueryRow("SELECT COUNT(*) FROM "+chunkTable).Scan(&chunksAfter))
	assert.Equal(t, chunksBefore, chunksAfter)

	// Container: no config.toml, only deployment variables.
	isolateDeploymentEnv(t)
	keyFile := filepath.Join(t.TempDir(), "embed-key")
	require.NoError(t, os.WriteFile(keyFile, []byte("container-key\n"), 0o600))
	t.Setenv("AGENTSVIEW_MODE", "pg-serve")
	t.Setenv("AGENTSVIEW_EMBEDDINGS_ENDPOINT", endpoint.URL+"/v1")
	t.Setenv("AGENTSVIEW_EMBEDDINGS_API_KEY_FILE", keyFile)
	container, err := config.LoadMinimal()
	require.NoError(t, err)
	require.False(t, container.Vector.Enabled)

	store, err := postgres.NewStore(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	mu.Lock()
	calls = nil
	mu.Unlock()
	require.NoError(t, wireReplicaVectorSearch(t.Context(), container, pgReplica{}, store, "pg serve"))

	// The seeded single-message sessions are one-shots, which the default
	// search scope hides.
	page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
		Pattern: "find", Mode: "semantic", Limit: 2, IncludeOneShot: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, page.Matches)
	assert.Equal(t, "session-one", page.Matches[0].SessionID)
	assert.Contains(t, page.Matches[0].Snippet, "nearest literal content")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 1, "one query embedding")
	assert.Equal(t, "Bearer container-key", calls[0].auth)
	assert.Equal(t, "published-model", calls[0].model)
	assert.Equal(t, "query: find", calls[0].input)
}
