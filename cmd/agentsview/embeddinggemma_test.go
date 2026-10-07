package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/vector"
)

func loadEmbeddingGemmaConfig(t *testing.T, endpoint string) config.Config {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	writeTestConfig(t, dataDir, fmt.Sprintf(`
[vector]
enabled = true
[vector.embeddings]
model = "embeddinggemma-2-914f7f89-text-768"
dimension = 768
request_dimensions = false
query_prefix = "task: search result | query: "
document_prefix = "title: none | text: "
model_context_tokens = 8192
[vector.embeddings.servers.local]
endpoint = %q
batch_size = 4
concurrency = 1
timeout = "120s"
max_retries = 0
`, endpoint))
	cfg, err := config.LoadMinimal()
	require.NoError(t, err)
	return cfg
}

type embeddingGemmaRequest struct {
	Method     string
	Path       string
	Model      string         `json:"model"`
	Input      []string       `json:"input"`
	Dimensions jsontext.Value `json:"dimensions"`
	InputType  jsontext.Value `json:"input_type"`
}

// These HTTP fixtures characterize AgentsView's transport, not model inference.
type embeddingGemmaRecorder struct {
	mu       sync.Mutex
	requests []embeddingGemmaRequest
}

func (r *embeddingGemmaRecorder) snapshot() []embeddingGemmaRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

func (r *embeddingGemmaRecorder) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body embeddingGemmaRequest
		if !assert.NoError(t, json.UnmarshalRead(req.Body, &body)) {
			return
		}
		body.Method, body.Path = req.Method, req.URL.Path
		r.mu.Lock()
		r.requests = append(r.requests, body)
		r.mu.Unlock()
		unit := make([]float32, 768)
		unit[0] = 1
		data := make([]map[string]any, len(body.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": unit}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": data}))
	}))
	t.Cleanup(server.Close)
	return server
}

// Dropping or doubling a configured role prompt must fail at the HTTP boundary.
func TestEmbeddingGemmaBuildAndSearch(t *testing.T) {
	recorder := &embeddingGemmaRecorder{}
	server := recorder.serve(t)
	cfg := loadEmbeddingGemmaConfig(t, server.URL+"/v1")
	seedEmbeddableArchive(t, cfg.DataDir)
	assert.Empty(t, cfg.Vector.Embeddings.InputSuffix)
	assert.False(t, cfg.Vector.Embeddings.RequestDimensions)
	transport, err := cfg.Vector.Embeddings.Servers["local"].EmbedTransport()
	require.NoError(t, err)
	assert.Equal(t, 120*time.Second, transport.Timeout)
	assert.Equal(t, 4, cfg.Vector.Embeddings.Servers["local"].BatchSize)

	var out bytes.Buffer
	require.NoError(t, runEmbeddingsBuildDirect(t.Context(), &out, cfg, vector.BuildRequest{}))
	// The direct build closes its index; search opens the persisted generation.
	database := dbtest.OpenTestDBAt(t, filepath.Join(cfg.DataDir, "sessions.db"))
	closeIndex := installDirectVectorSearcher(cfg, database)
	require.NotNil(t, closeIndex)
	filter := db.ContentSearchFilter{
		Pattern: "find greeting", Mode: "semantic", Limit: 5, IncludeOneShot: true,
	}
	result, err := database.SearchContent(t.Context(), filter)
	require.NoError(t, err)
	require.NotEmpty(t, result.Matches)
	require.NoError(t, closeIndex())

	requests := recorder.snapshot()
	var inputs []string
	queryRequests := 0
	for _, request := range requests {
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/v1/embeddings", request.Path)
		assert.Equal(t, "embeddinggemma-2-914f7f89-text-768", request.Model)
		assert.Nil(t, request.Dimensions)
		assert.Nil(t, request.InputType)
		inputs = append(inputs, request.Input...)
		if slices.Contains(request.Input, "task: search result | query: find greeting") {
			queryRequests++
		}
	}
	assert.ElementsMatch(t, []string{
		"title: none | text: hello there",
		"title: none | text: hi back\n\nand a follow-up thought",
		"task: search result | query: find greeting",
	}, inputs)
	require.Equal(t, 1, queryRequests, "same-config control encodes exactly one query")

	for _, change := range []struct {
		name  string
		apply func(*config.VectorEmbeddingsConfig)
	}{
		{"query prefix", func(c *config.VectorEmbeddingsConfig) { c.QueryPrefix = "query: " }},
		{"document prefix", func(c *config.VectorEmbeddingsConfig) { c.DocumentPrefix = "document: " }},
		{"suffix", func(c *config.VectorEmbeddingsConfig) { c.InputSuffix = "<eos>" }},
		{"requested dimensions", func(c *config.VectorEmbeddingsConfig) { c.RequestDimensions = true }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := cfg
			change.apply(&changed.Vector.Embeddings)
			closeChanged := installDirectVectorSearcher(changed, database)
			require.NotNil(t, closeChanged)
			t.Cleanup(func() { require.NoError(t, closeChanged()) })
			before := len(recorder.snapshot())
			result, err := database.SearchContent(t.Context(), filter)
			require.ErrorIs(t, err, db.ErrSemanticUnavailable)
			assert.Empty(t, result.Matches)
			assert.Len(t, recorder.snapshot(), before, "stale generation rejects before query egress")
		})
	}
}

// AgentsView chooses to preserve non-unit vectors; the serving recipe must
// supply normalization. Removing width/numeric validation must fail these cases.
func TestEmbeddingGemmaResponseContract(t *testing.T) {
	nonunit := make([]float32, 768)
	nonunit[0], nonunit[1] = 3, 4
	for _, factory := range []struct {
		name string
		new  func(config.VectorEmbeddingsConfig, string) (kitvec.EncodeFunc, error)
	}{
		{"document", newVectorDocumentEncoder},
		{"query", newVectorQueryEncoder},
	} {
		for _, fixture := range []struct {
			name string
			vec  any
			err  string
		}{
			{"nonunit preserved", nonunit, ""},
			{"short", append([]float32{1}, make([]float32, 766)...), "767 dimensions"},
			{"oversized", append([]float32{1}, make([]float32, 768)...), "769 dimensions"},
			{"zero", make([]float32, 768), "zero norm"},
			{"float32 overflow", append([]float64{1e39, 1}, make([]float64, 766)...), "not finite"},
			{"base64 NaN", embeddingGemmaBase64(math.Float32frombits(0x7fc00000)), "not finite"},
			{"base64 infinity", embeddingGemmaBase64(float32(math.Inf(1))), "not finite"},
		} {
			t.Run(factory.name+"/"+fixture.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": []map[string]any{
						{"index": 0, "embedding": fixture.vec},
					}}))
				}))
				t.Cleanup(server.Close)
				cfg := loadEmbeddingGemmaConfig(t, server.URL+"/v1")
				encoder, err := factory.new(cfg.Vector.Embeddings, "")
				require.NoError(t, err)
				vectors, err := encoder(t.Context(), []string{"synthetic text"})
				if fixture.err != "" {
					require.ErrorContains(t, err, fixture.err)
					assert.Nil(t, vectors, "invalid output must not leave usable vectors")
					return
				}
				require.NoError(t, err)
				assert.Equal(t, [][]float32{nonunit}, vectors)
			})
		}
	}
}

func embeddingGemmaBase64(component float32) string {
	data := make([]byte, 768*4)
	binary.LittleEndian.PutUint32(data, math.Float32bits(component))
	binary.LittleEndian.PutUint32(data[4:], math.Float32bits(1))
	return base64.StdEncoding.EncodeToString(data)
}

func TestEmbeddingGemmaRequestedDimensions(t *testing.T) {
	for _, factory := range []struct {
		name string
		new  func(config.VectorEmbeddingsConfig, string) (kitvec.EncodeFunc, error)
	}{
		{"document", newVectorDocumentEncoder}, {"query", newVectorQueryEncoder},
	} {
		t.Run(factory.name, func(t *testing.T) {
			var width atomic.Int32
			width.Store(256)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body embeddingGemmaRequest
				if !assert.NoError(t, json.UnmarshalRead(req.Body, &body)) {
					return
				}
				assert.JSONEq(t, "256", string(body.Dimensions))
				values := make([]float32, int(width.Load()))
				values[0] = 1
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"data": []map[string]any{
					{"index": 0, "embedding": values},
				}}))
			}))
			t.Cleanup(server.Close)
			cfg := loadEmbeddingGemmaConfig(t, server.URL+"/v1")
			cfg.Vector.Embeddings.Dimension = 256
			cfg.Vector.Embeddings.RequestDimensions = true
			encoder, err := factory.new(cfg.Vector.Embeddings, "")
			require.NoError(t, err)
			vectors, err := encoder(t.Context(), []string{"synthetic text"})
			require.NoError(t, err)
			expected := make([]float32, 256)
			expected[0] = 1
			assert.Equal(t, [][]float32{expected}, vectors)
			width.Store(769)
			vectors, err = encoder(t.Context(), []string{"synthetic text"})
			require.ErrorContains(t, err, "769 dimensions, expected 256")
			assert.Nil(t, vectors)
		})
	}
}
