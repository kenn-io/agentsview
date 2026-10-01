package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/vector"
)

// TestVectorGenerationFingerprintsMatchPreKitRelease pins the generation
// fingerprints to the values the release before the kit embedding stack
// computed for the same configuration, so upgrading keeps every stored
// generation current instead of scheduling a re-embed.
func TestVectorGenerationFingerprintsMatchPreKitRelease(t *testing.T) {
	plain := config.VectorEmbeddingsConfig{
		Model: "nomic-embed-text", Dimension: 768, MaxInputChars: 8192,
	}
	full := config.VectorEmbeddingsConfig{
		Model: "qwen3-embedding", Dimension: 1024, MaxInputChars: 4000,
		RequestDimensions: true,
		QueryPrefix:       "Instruct: find\nQuery: ",
		DocumentPrefix:    "doc: ",
		InputSuffix:       "<|endoftext|>",
	}
	tests := []struct {
		name string
		cfg  config.VectorEmbeddingsConfig
		gen  func(config.VectorEmbeddingsConfig) string
		want string
	}{
		{"messages", plain, func(c config.VectorEmbeddingsConfig) string {
			return vectorGeneration(c).Fingerprint()
		}, "97856d475cdedc12"},
		{"messages with every recipe key", full, func(c config.VectorEmbeddingsConfig) string {
			return vectorGeneration(c).Fingerprint()
		}, "d6535c8c0b152c2f"},
		{"recall", plain, func(c config.VectorEmbeddingsConfig) string {
			return recallVectorGeneration(c, "extract-v1").Fingerprint()
		}, "f5344001f763b46d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.gen(tt.cfg))
		})
	}

	space := vectorSpace(plain, vectorGeneration(plain))
	matches, err := space.Matches("97856d475cdedc12")
	require.NoError(t, err)
	assert.True(t, matches, "the descriptor owns the stored generation")

	for name, changed := range map[string]config.VectorEmbeddingsConfig{
		"model":           {Model: "other-model", Dimension: 768, MaxInputChars: 8192},
		"chunk recipe":    {Model: "nomic-embed-text", Dimension: 768, MaxInputChars: 4096},
		"document prefix": {Model: "nomic-embed-text", Dimension: 768, MaxInputChars: 8192, DocumentPrefix: "d: "},
	} {
		changedSpace := vectorSpace(changed, vectorGeneration(changed))
		matches, err := changedSpace.Matches("97856d475cdedc12")
		require.NoError(t, err)
		assert.False(t, matches, "a %s change must cut a new generation", name)
	}
}

// recordingEmbeddings is an OpenAI-compatible endpoint that answers every
// input with the same unit vector and records the inputs it received.
type recordingEmbeddings struct {
	mu     sync.Mutex
	inputs []string
}

func (r *recordingEmbeddings) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(req.Body, &body)) {
			return
		}
		r.mu.Lock()
		r.inputs = append(r.inputs, body.Input...)
		r.mu.Unlock()
		data := make([]map[string]any, len(body.Input))
		for i := range body.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0, 0}}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *recordingEmbeddings) received() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.inputs)
}

// TestKitMigrationReusesExistingGenerationForIndexingAndSearch reopens a
// vectors.db whose active generation was stored under the existing
// fingerprint and drives it through the kit-backed encoders: indexing sends
// no document text, search serves the stored vectors, an edited document is
// the only one re-embedded, and a deleted document stops matching.
func TestKitMigrationReusesExistingGenerationForIndexingAndSearch(t *testing.T) {
	ctx := t.Context()
	provider := &recordingEmbeddings{}
	srv := provider.serve(t)

	cfg := enabledVectorConfig(t)
	cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{
		"local": {
			Endpoint: srv.URL + "/v1", Timeout: "5s",
			BatchSize: 32, Concurrency: 1, MaxRetries: 1,
		},
	}
	path := cfg.Vector.ResolvedDBPath(cfg.DataDir)
	gen := vectorGeneration(cfg.Vector.Embeddings)
	src := testPushUnitSource()

	// The existing index, built before the kit client was adopted.
	existing, err := vector.Open(ctx, path, false, cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	_, err = existing.Build(ctx, src, fakePushEncoder(), gen, vector.BuildOptions{})
	require.NoError(t, err)
	require.NoError(t, existing.Close())

	ix, err := vector.Open(ctx, path, false, cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	encoders, err := vectorDocumentEncoderSet(cfg.Vector.Embeddings)
	require.NoError(t, err)
	space := vectorSpace(cfg.Vector.Embeddings, gen)
	var source vector.UnitSource = src
	manager := vector.NewResolvingManager(ix, encoders, gen,
		func(context.Context) (vector.BuildTarget, error) {
			return vector.BuildTarget{Source: source, Generation: gen, Space: space}, nil
		})

	ran, err := manager.TryBuild(ctx, vector.BuildRequest{Backstop: true})
	require.NoError(t, err)
	require.True(t, ran)
	assert.Empty(t, provider.received(), "existing vectors are reused, not re-embedded")
	generations, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, generations, 1, "no new generation is scheduled")
	assert.Equal(t, gen.Fingerprint(), generations[0].Fingerprint)
	assert.Equal(t, "active", generations[0].State)

	queryEncoder, err := newVectorQueryEncoder(cfg.Vector.Embeddings, "")
	require.NoError(t, err)
	searcher := newSearcherAdapter(ix, queryEncoder, space)
	hits, err := searcher.SemanticSearch(ctx, "hello", 10)
	require.NoError(t, err, "the stored generation is not reported stale")
	assert.Len(t, hits, 3)
	assert.Equal(t, []string{"hello"}, provider.received(), "only the query is embedded")

	edited := testPushUnitSource()
	edited.units[0].Content = "hello again"
	edited.units = edited.units[:2] // session-2's document was deleted
	source = edited
	_, err = manager.TryBuild(ctx, vector.BuildRequest{Backstop: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"hello", "hello again"}, provider.received(),
		"only the edited document is re-embedded")

	hits, err = searcher.SemanticSearch(ctx, "hello", 10)
	require.NoError(t, err)
	sessions := make([]string, 0, len(hits))
	for _, hit := range hits {
		sessions = append(sessions, hit.SessionID)
	}
	assert.NotContains(t, sessions, "session-2", "a deleted document no longer matches")
	assert.Len(t, hits, 2)
}
