package vector

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// embeddingsRequest mirrors the OpenAI-compatible request body the encoder
// sends, for use in test assertions.
type embeddingsRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

// embeddingDatum mirrors one element of the OpenAI-compatible response.
type embeddingDatum struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embeddingsResponse struct {
	Data []embeddingDatum `json:"data"`
}

type testOllamaEmbedRequest struct {
	Model      string         `json:"model"`
	Input      []string       `json:"input"`
	Truncate   *bool          `json:"truncate"`
	Dimensions *int           `json:"dimensions,omitempty"`
	Options    map[string]any `json:"options"`
	KeepAlive  string         `json:"keep_alive"`
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.MarshalWrite(w, v))
}

func TestEmbeddingURLsPreserveEndpointComponents(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    string
		wantPrimary string
		wantNative  string
	}{
		{
			name:        "plain endpoint",
			endpoint:    "https://example.test/v1",
			wantPrimary: "https://example.test/v1/embeddings",
			wantNative:  "https://example.test/api/embed",
		},
		{
			name:        "proxy prefix trailing slash and query",
			endpoint:    "https://example.test/proxy/v1/?tenant=local",
			wantPrimary: "https://example.test/proxy/v1/embeddings?tenant=local",
			wantNative:  "https://example.test/proxy/api/embed?tenant=local",
		},
		{
			name:        "encoded proxy separator",
			endpoint:    "https://example.test/proxy%2Ftenant/v1?tenant=local",
			wantPrimary: "https://example.test/proxy%2Ftenant/v1/embeddings?tenant=local",
			wantNative:  "https://example.test/proxy%2Ftenant/api/embed?tenant=local",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPrimary, err := openAIEmbeddingsURL(tt.endpoint)
			require.NoError(t, err)
			assert.Equal(t, tt.wantPrimary, gotPrimary)

			gotNative, err := ollamaEmbedURL(tt.endpoint)
			require.NoError(t, err)
			assert.Equal(t, tt.wantNative, gotNative)
		})
	}
}

func TestOllamaModelNamesMatchProcessDisplayNames(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		loaded     string
		want       bool
	}{
		{name: "exact", configured: "model:tag", loaded: "model:tag", want: true},
		{name: "implicit latest", configured: "model", loaded: "model:latest", want: true},
		{
			name: "default registry and namespace", configured: "registry.ollama.ai/library/model:tag",
			loaded: "model:tag", want: true,
		},
		{
			name:       "protocol-qualified default registry",
			configured: "https://registry.ollama.ai/library/model:tag",
			loaded:     "model:tag", want: true,
		},
		{
			name: "default registry", configured: "registry.ollama.ai/team/model:tag",
			loaded: "team/model:tag", want: true,
		},
		{
			name: "different registries", configured: "host-a.example/team/model:tag",
			loaded: "host-b.example/team/model:tag", want: false,
		},
		{
			name: "different namespaces", configured: "team-a/model:tag",
			loaded: "team-b/model:tag", want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ollamaModelNamesMatch(tt.configured, tt.loaded))
		})
	}
}

func TestEncoderHappyPath(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotReq embeddingsRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &gotReq))

		// Return data out of order to verify reordering by index.
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{
				{Index: 1, Embedding: []float32{4, 5, 6}},
				{Index: 0, Embedding: []float32{1, 2, 3}},
			},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		APIKey:     "secret-key",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 3,
	})

	out, err := enc(context.Background(), []string{"hello", "world"})
	require.NoError(t, err)

	assert.Equal(t, "/v1/embeddings", gotPath)
	assert.Equal(t, "Bearer secret-key", gotAuth)
	assert.Equal(t, "test-model", gotReq.Model)
	assert.Equal(t, []string{"hello", "world"}, gotReq.Input)
	assert.Equal(t, "base64", gotReq.EncodingFormat,
		"requests ask for the compact base64 wire format")

	require.Len(t, out, 2)
	assert.Equal(t, []float32{1, 2, 3}, out[0])
	assert.Equal(t, []float32{4, 5, 6}, out[1])
}

// base64Embedding encodes floats the way encoding_format "base64" responses
// carry them: raw little-endian float32 bytes, base64-encoded.
func base64Embedding(floats []float32) string {
	raw := make([]byte, 4*len(floats))
	for i, f := range floats {
		binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(f))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// TestEncoderDecodesBase64Embeddings asserts a server honoring
// encoding_format "base64" round-trips to the same float vectors, including
// reordering by index.
func TestEncoderDecodesBase64Embeddings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"index": 1, "embedding": base64Embedding([]float32{4, 5, 6})},
				{"index": 0, "embedding": base64Embedding([]float32{1, 2, 3})},
			},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:  srv.URL + "/v1",
		Model:     "test-model",
		Dimension: 3,
		Timeout:   5 * time.Second,
	})

	out, err := enc(context.Background(), []string{"hello", "world"})
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, []float32{1, 2, 3}, out[0])
	assert.Equal(t, []float32{4, 5, 6}, out[1])
}

func TestEncoderRejectsInvalidBase64Embeddings(t *testing.T) {
	tests := []struct {
		name      string
		embedding []float32
		reason    string
	}{
		{name: "nan", embedding: []float32{1, float32(math.NaN()), 0}, reason: "non-finite"},
		{name: "positive infinity", embedding: []float32{1, float32(math.Inf(1)), 0}, reason: "non-finite"},
		{name: "negative infinity", embedding: []float32{1, float32(math.Inf(-1)), 0}, reason: "non-finite"},
		{name: "zero norm", embedding: []float32{0, 0, 0}, reason: "zero norm"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"data": []map[string]any{{
						"index": 0, "embedding": base64Embedding(tt.embedding),
					}},
				})
			}))
			defer srv.Close()

			enc := NewEncoder(EncoderConfig{
				Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
				Timeout: time.Second, MaxRetries: 1,
			})
			out, err := enc(context.Background(), []string{"hello"})

			assert.Nil(t, out, "an invalid batch must expose no partial vectors")
			var invalidErr *InvalidEmbeddingError
			require.ErrorAs(t, err, &invalidErr)
			assert.Equal(t, 0, invalidErr.Index)
			assert.Contains(t, err.Error(), tt.reason)
		})
	}
}

func TestEncoderRejectsNullEmbeddingElements(t *testing.T) {
	tests := []struct {
		name      string
		embedding string
	}{
		{name: "mixed", embedding: `[0.5,null,0.25]`},
		{name: "all null", embedding: `[null,null,null]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, err := io.WriteString(w, `{"data":[{"index":0,"embedding":`+tt.embedding+`}]}`)
				require.NoError(t, err)
			}))
			defer srv.Close()

			enc := NewEncoder(EncoderConfig{
				Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
				Timeout: time.Second, MaxRetries: 1,
			})
			out, err := enc(context.Background(), []string{"hello"})

			assert.Nil(t, out)
			require.ErrorContains(t, err, "null")
		})
	}
}

func TestEncoderRetriesInvalidEmbeddingResponse(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			writeJSON(t, w, http.StatusOK, map[string]any{
				"data": []map[string]any{{
					"index": 0, "embedding": base64Embedding([]float32{0, 0, 0}),
				}},
			})
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"data": []map[string]any{{
				"index": 0, "embedding": base64Embedding([]float32{1, 2, 3}),
			}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: time.Second, MaxRetries: 2,
	})
	out, err := enc(context.Background(), []string{"hello"})

	require.NoError(t, err)
	assert.Equal(t, int32(2), requests.Load())
	assert.Equal(t, [][]float32{{1, 2, 3}}, out)
}

func TestEncoderOllamaFallbackReloadsMetalBeforeCPU(t *testing.T) {
	var nativeRequests []testOllamaEmbedRequest
	var unloadChecks int
	runnerLoaded := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
			}})
		case "/api/embed":
			var request testOllamaEmbedRequest
			require.NoError(t, json.UnmarshalRead(r.Body, &request))
			nativeRequests = append(nativeRequests, request)
			switch len(nativeRequests) {
			case 1:
				runnerLoaded = true
				writeJSON(t, w, http.StatusOK, map[string]any{
					"embeddings": [][]float32{{0, 0, 0}},
				})
			case 2:
				if runnerLoaded {
					writeJSON(t, w, http.StatusOK, map[string]any{
						"embeddings": [][]float32{{0, 0, 0}},
					})
					return
				}
				writeJSON(t, w, http.StatusOK, map[string]any{
					"embeddings": [][]float32{{4, 5, 6}},
				})
			default:
				http.Error(w, "unexpected CPU fallback", http.StatusInternalServerError)
			}
		case "/api/ps":
			unloadChecks++
			if unloadChecks == 1 {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"models": []map[string]any{{"model": "test-model"}},
				})
				return
			}
			runnerLoaded = false
			writeJSON(t, w, http.StatusOK, map[string]any{"models": []any{}})
		default:
			require.FailNow(t, "unexpected request path", r.URL.Path)
		}
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: time.Second, MaxRetries: 1, OllamaCPUFallback: true,
	})
	out, err := enc(context.Background(), []string{"alpha"})

	require.NoError(t, err)
	assert.Equal(t, [][]float32{{4, 5, 6}}, out)
	require.Len(t, nativeRequests, 2)
	assert.Nil(t, nativeRequests[0].Options, "the unload request must stay on Metal")
	assert.Equal(t, "0s", nativeRequests[0].KeepAlive)
	assert.Nil(t, nativeRequests[1].Options, "the fresh retry must stay on Metal")
	assert.Empty(t, nativeRequests[1].KeepAlive, "the fresh runner should remain loaded")
	assert.Equal(t, 2, unloadChecks)
}

func TestEncoderOllamaCPUFallbackReplacesOnlyInvalidVectors(t *testing.T) {
	var metalCalls, nativeCalls atomic.Int32
	var gotNative []testOllamaEmbedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/v1/embeddings":
			metalCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{1, 2, 3})},
				{"index": 1, "embedding": base64Embedding([]float32{0, 0, 0})},
				{"index": 2, "embedding": base64Embedding(
					[]float32{1, float32(math.NaN()), 0})},
			}})
		case "/proxy/api/embed":
			call := nativeCalls.Add(1)
			require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
			var request testOllamaEmbedRequest
			require.NoError(t, json.UnmarshalRead(r.Body, &request))
			gotNative = append(gotNative, request)
			if call < 3 {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"embeddings": [][]float32{{0, 0, 0}, {0, 0, 0}},
				})
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]any{
				"model":      "test-model",
				"embeddings": [][]float32{{4, 5, 6}, {7, 8, 9}},
			})
		case "/proxy/api/ps":
			require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
			writeJSON(t, w, http.StatusOK, map[string]any{"models": []any{}})
		default:
			require.FailNow(t, "unexpected request path", r.URL.Path)
		}
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/proxy/v1", APIKey: "secret",
		Model: "test-model", Dimension: 3, RequestDimensions: true,
		Timeout: time.Second, MaxRetries: 2,
		InputPrefix: "pre:", InputSuffix: ":suf",
		OllamaCPUFallback: true,
	})
	out, err := enc(context.Background(), []string{"alpha", "beta", "gamma"})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{1, 2, 3}, {4, 5, 6}, {7, 8, 9}}, out)
	assert.Equal(t, int32(2), metalCalls.Load(), "normal retries run first")
	require.Len(t, gotNative, 3)
	for _, request := range gotNative {
		assert.Equal(t, "test-model", request.Model)
		assert.Equal(t, []string{"pre:beta:suf", "pre:gamma:suf"}, request.Input)
		require.NotNil(t, request.Truncate, "truncate must be present explicitly")
		assert.False(t, *request.Truncate)
		require.NotNil(t, request.Dimensions)
		assert.Equal(t, 3, *request.Dimensions)
	}
	assert.Nil(t, gotNative[0].Options, "the unload request must stay on Metal")
	assert.Equal(t, "0s", gotNative[0].KeepAlive)
	assert.Nil(t, gotNative[1].Options, "the fresh retry must stay on Metal")
	assert.Empty(t, gotNative[1].KeepAlive)
	assert.EqualValues(t, 0, gotNative[2].Options["num_gpu"])
	assert.Equal(t, "0s", gotNative[2].KeepAlive)
}

func TestEncoderOllamaCPUFallbackOmitsDimensionsWhenNotRequested(t *testing.T) {
	var gotNative []testOllamaEmbedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
			}})
		case "/api/embed":
			var request testOllamaEmbedRequest
			require.NoError(t, json.UnmarshalRead(r.Body, &request))
			gotNative = append(gotNative, request)
			if len(gotNative) < 3 {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"embeddings": [][]float32{{0, 0, 0}},
				})
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]any{
				"model":      "test-model",
				"embeddings": [][]float32{{1, 2, 3}},
			})
		case "/api/ps":
			writeJSON(t, w, http.StatusOK, map[string]any{"models": []any{}})
		default:
			require.FailNow(t, "unexpected request path", r.URL.Path)
		}
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: time.Second, MaxRetries: 1, OllamaCPUFallback: true,
	})
	out, err := enc(context.Background(), []string{"alpha"})
	require.NoError(t, err)
	assert.Equal(t, [][]float32{{1, 2, 3}}, out)
	require.Len(t, gotNative, 3)
	for _, request := range gotNative {
		require.NotNil(t, request.Truncate, "truncate must be present explicitly")
		assert.False(t, *request.Truncate)
		assert.Nil(t, request.Dimensions, "dimensions must be omitted unless requested")
	}
}

func TestEncoderOllamaCPUFallbackPreservesQueryOnBothRoutes(t *testing.T) {
	var primaryCalls, nativeCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "local", r.URL.Query().Get("tenant"))
		switch r.URL.Path {
		case "/proxy/v1/embeddings":
			primaryCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
			}})
		case "/proxy/api/embed":
			call := nativeCalls.Add(1)
			if call < 3 {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"embeddings": [][]float32{{0, 0, 0}},
				})
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]any{
				"embeddings": [][]float32{{1, 2, 3}},
			})
		case "/proxy/api/ps":
			writeJSON(t, w, http.StatusOK, map[string]any{"models": []any{}})
		default:
			http.Error(w, "unexpected request path", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/proxy/v1/?tenant=local",
		Model:    "test-model", Dimension: 3, Timeout: time.Second,
		MaxRetries: 1, OllamaCPUFallback: true,
	})
	out, err := enc(context.Background(), []string{"alpha"})

	require.NoError(t, err)
	assert.Equal(t, [][]float32{{1, 2, 3}}, out)
	assert.Equal(t, int32(1), primaryCalls.Load())
	assert.Equal(t, int32(3), nativeCalls.Load())
}

func TestEncoderOllamaCPUFallbackQuiescesSharedEndpointTraffic(t *testing.T) {
	var primaryCalls, nativeCalls atomic.Int32
	cpuStarted := make(chan struct{})
	releaseCPU := make(chan struct{})
	secondPrimaryStarted := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			call := primaryCalls.Add(1)
			if call == 1 {
				writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
					{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
				}})
				return
			}
			secondPrimaryStarted <- struct{}{}
			writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{4, 5, 6})},
			}})
		case "/api/embed":
			if nativeCalls.Add(1) < 3 {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"embeddings": [][]float32{{0, 0, 0}},
				})
				return
			}
			close(cpuStarted)
			<-releaseCPU
			writeJSON(t, w, http.StatusOK, map[string]any{
				"embeddings": [][]float32{{1, 2, 3}},
			})
		case "/api/ps":
			writeJSON(t, w, http.StatusOK, map[string]any{"models": []any{}})
		default:
			require.FailNow(t, "unexpected request path", r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: time.Second, MaxRetries: 1, OllamaCPUFallback: true,
	}
	firstEncoder := NewEncoder(cfg)
	secondEncoder := NewEncoder(cfg)
	firstDone := make(chan error, 1)
	go func() {
		_, err := firstEncoder(context.Background(), []string{"alpha"})
		firstDone <- err
	}()
	<-cpuStarted

	secondDone := make(chan error, 1)
	go func() {
		_, err := secondEncoder(context.Background(), []string{"beta"})
		secondDone <- err
	}()
	primaryEscapedGate := false
	select {
	case <-secondPrimaryStarted:
		primaryEscapedGate = true
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseCPU)
	require.NoError(t, <-firstDone)
	if !primaryEscapedGate {
		select {
		case <-secondPrimaryStarted:
		case <-time.After(time.Second):
			t.Fatal("primary request did not resume after the CPU fallback completed")
		}
	}
	require.NoError(t, <-secondDone)
	assert.False(t, primaryEscapedGate,
		"primary request reached Ollama while the CPU fallback held the endpoint")
	assert.Equal(t, int32(2), primaryCalls.Load())
}

func TestEncoderOllamaGatePrimaryWaitHonorsCancellation(t *testing.T) {
	var primaryCalls, nativeCalls atomic.Int32
	cpuStarted := make(chan struct{})
	releaseCPU := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			primaryCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
			}})
		case "/api/embed":
			if nativeCalls.Add(1) < 3 {
				writeJSON(t, w, http.StatusOK, map[string]any{
					"embeddings": [][]float32{{0, 0, 0}},
				})
				return
			}
			close(cpuStarted)
			<-releaseCPU
			writeJSON(t, w, http.StatusOK, map[string]any{
				"embeddings": [][]float32{{1, 2, 3}},
			})
		case "/api/ps":
			writeJSON(t, w, http.StatusOK, map[string]any{"models": []any{}})
		default:
			require.FailNow(t, "unexpected request path", r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: 5 * time.Second, MaxRetries: 1, OllamaCPUFallback: true,
	}
	firstEncoder := NewEncoder(cfg)
	secondEncoder := NewEncoder(cfg)
	firstDone := make(chan error, 1)
	go func() {
		_, err := firstEncoder(context.Background(), []string{"alpha"})
		firstDone <- err
	}()
	<-cpuStarted

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := secondEncoder(ctx, []string{"beta"})
		secondDone <- err
	}()
	cancel()

	select {
	case err := <-secondDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		close(releaseCPU)
		require.NoError(t, <-firstDone)
		<-secondDone
		t.Fatal("canceled primary request remained blocked behind CPU fallback")
	}

	close(releaseCPU)
	require.NoError(t, <-firstDone)
	assert.Equal(t, int32(1), primaryCalls.Load())
}

func TestEncoderOllamaGateFallbackWaitHonorsCancellation(t *testing.T) {
	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	invalidSent := make(chan struct{})
	var cpuCalls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			var req embeddingsRequest
			require.NoError(t, json.UnmarshalRead(r.Body, &req))
			require.Len(t, req.Input, 1)
			if req.Input[0] == "blocker" {
				close(blockerStarted)
				<-releaseBlocker
				writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
					{"index": 0, "embedding": base64Embedding([]float32{1, 2, 3})},
				}})
				return
			}
			writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
			}})
			close(invalidSent)
		case "/api/embed":
			cpuCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{
				"embeddings": [][]float32{{4, 5, 6}},
			})
		default:
			require.FailNow(t, "unexpected request path", r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: 5 * time.Second, MaxRetries: 1, OllamaCPUFallback: true,
	}
	blocker := NewEncoder(cfg)
	repair := NewEncoder(cfg)
	blockerDone := make(chan error, 1)
	go func() {
		_, err := blocker(context.Background(), []string{"blocker"})
		blockerDone <- err
	}()
	<-blockerStarted

	ctx, cancel := context.WithCancel(context.Background())
	repairDone := make(chan error, 1)
	go func() {
		_, err := repair(ctx, []string{"repair"})
		repairDone <- err
	}()
	<-invalidSent
	cancel()

	select {
	case err := <-repairDone:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		close(releaseBlocker)
		require.NoError(t, <-blockerDone)
		<-repairDone
		t.Fatal("canceled CPU fallback remained blocked behind a primary request")
	}

	close(releaseBlocker)
	require.NoError(t, <-blockerDone)
	assert.Equal(t, int32(0), cpuCalls.Load())
}

func TestEncoderOllamaCPUFallbackDisabledLeavesInvalidResponseFailed(t *testing.T) {
	var cpuCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
			}})
		case "/api/embed":
			cpuCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{
				"embeddings": [][]float32{{1, 2, 3}},
			})
		default:
			require.FailNow(t, "unexpected request path", r.URL.Path)
		}
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: time.Second, MaxRetries: 1,
	})
	out, err := enc(context.Background(), []string{"alpha"})

	assert.Nil(t, out)
	var invalidErr *InvalidEmbeddingError
	require.ErrorAs(t, err, &invalidErr)
	assert.Equal(t, int32(0), cpuCalls.Load())
}

func TestEncoderOllamaCPUFallbackDoesNotMaskDimensionMismatch(t *testing.T) {
	var cpuCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/embeddings":
			writeJSON(t, w, http.StatusOK, embeddingsResponse{
				Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2}}},
			})
		case "/api/embed":
			cpuCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{
				"embeddings": [][]float32{{1, 2, 3}},
			})
		default:
			require.FailNow(t, "unexpected request path", r.URL.Path)
		}
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: time.Second, MaxRetries: 1, OllamaCPUFallback: true,
	})
	out, err := enc(context.Background(), []string{"alpha"})

	assert.Nil(t, out)
	require.ErrorContains(t, err, "dimension mismatch")
	assert.Equal(t, int32(0), cpuCalls.Load())
}

func TestEncoderOllamaCPUFallbackRejectsIneligiblePrimaryFailures(t *testing.T) {
	tests := []struct {
		name    string
		respond func(*testing.T, http.ResponseWriter)
		wantErr string
	}{
		{
			name: "authentication failure",
			respond: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				http.Error(w, "invalid token", http.StatusUnauthorized)
			},
			wantErr: "status 401",
		},
		{
			name: "route or model failure",
			respond: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				http.Error(w, "model not found", http.StatusNotFound)
			},
			wantErr: "status 404",
		},
		{
			name: "malformed response",
			respond: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				w.Header().Set("Content-Type", "application/json")
				_, err := io.WriteString(w, "{not valid json")
				require.NoError(t, err)
			},
			wantErr: "decode response",
		},
		{
			name: "response count mismatch",
			respond: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				writeJSON(t, w, http.StatusOK, embeddingsResponse{})
			},
			wantErr: "count mismatch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cpuCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/embeddings":
					tt.respond(t, w)
				case "/api/embed":
					cpuCalls.Add(1)
					http.Error(w, "unexpected CPU fallback", http.StatusInternalServerError)
				default:
					require.FailNow(t, "unexpected request path", r.URL.Path)
				}
			}))
			defer srv.Close()

			enc := NewEncoder(EncoderConfig{
				Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
				Timeout: time.Second, MaxRetries: 1, OllamaCPUFallback: true,
			})
			out, err := enc(context.Background(), []string{"alpha"})

			assert.Nil(t, out)
			require.ErrorContains(t, err, tt.wantErr)
			assert.Equal(t, int32(0), cpuCalls.Load())
		})
	}
}

func TestEncoderOllamaCPUFallbackFailureLeavesBatchFailed(t *testing.T) {
	tests := []struct {
		name       string
		texts      []string
		primary    []map[string]any
		respondCPU func(*testing.T, http.ResponseWriter)
		wantErr    string
	}{
		{
			name:  "native status",
			texts: []string{"alpha"},
			primary: []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
			},
			respondCPU: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				http.Error(w, "CPU runner failed", http.StatusInternalServerError)
			},
			wantErr: "status 500",
		},
		{
			name:  "wrong response count",
			texts: []string{"alpha"},
			primary: []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
			},
			respondCPU: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				writeJSON(t, w, http.StatusOK, map[string]any{"embeddings": [][]float32{}})
			},
			wantErr: "count mismatch",
		},
		{
			name:  "wrong vector dimension",
			texts: []string{"alpha"},
			primary: []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
			},
			respondCPU: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				writeJSON(t, w, http.StatusOK, map[string]any{
					"embeddings": [][]float32{{1, 2}},
				})
			},
			wantErr: "dimension mismatch",
		},
		{
			name:  "zero norm preserves original index",
			texts: []string{"alpha", "beta"},
			primary: []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{1, 2, 3})},
				{"index": 1, "embedding": base64Embedding([]float32{0, 0, 0})},
			},
			respondCPU: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				writeJSON(t, w, http.StatusOK, map[string]any{
					"embeddings": [][]float32{{0, 0, 0}},
				})
			},
			wantErr: "index 1",
		},
		{
			name:  "null component",
			texts: []string{"alpha"},
			primary: []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{0, 0, 0})},
			},
			respondCPU: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				w.Header().Set("Content-Type", "application/json")
				_, err := io.WriteString(w, `{"embeddings":[[1,null,2]]}`)
				require.NoError(t, err)
			},
			wantErr: "null",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cpuCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/embeddings":
					writeJSON(t, w, http.StatusOK, map[string]any{"data": tt.primary})
				case "/api/embed":
					var request testOllamaEmbedRequest
					require.NoError(t, json.UnmarshalRead(r.Body, &request))
					if request.Options == nil {
						writeJSON(t, w, http.StatusOK, map[string]any{
							"embeddings": [][]float32{{0, 0, 0}},
						})
						return
					}
					cpuCalls.Add(1)
					tt.respondCPU(t, w)
				case "/api/ps":
					writeJSON(t, w, http.StatusOK, map[string]any{"models": []any{}})
				default:
					require.FailNow(t, "unexpected request path", r.URL.Path)
				}
			}))
			defer srv.Close()

			enc := NewEncoder(EncoderConfig{
				Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
				Timeout: time.Second, MaxRetries: 1, OllamaCPUFallback: true,
			})
			out, err := enc(context.Background(), tt.texts)

			assert.Nil(t, out, "a failed fallback must expose no partial vectors")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "Ollama CPU fallback")
			assert.Contains(t, err.Error(), tt.wantErr)
			var invalidErr *InvalidEmbeddingError
			require.ErrorAs(t, err, &invalidErr,
				"the original Metal invalid-vector error must remain available")
			assert.Equal(t, int32(1), cpuCalls.Load(), "CPU is attempted exactly once")
		})
	}
}

// TestEncoderBase64WrongByteCountFailsDimensionCheck asserts a base64
// payload whose float count disagrees with the configured dimension is
// rejected rather than silently stored.
func TestEncoderBase64WrongByteCountFailsDimensionCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": base64Embedding([]float32{1, 2})},
			},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:  srv.URL + "/v1",
		Model:     "test-model",
		Dimension: 3,
		Timeout:   5 * time.Second,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.ErrorContains(t, err, "dimension mismatch")
}

// TestEncoderFallsBackToFloatsWhenBase64Rejected asserts that a server which
// 400s the encoding_format field triggers a permanent downgrade: the request
// is redone without the field immediately, and later calls never ask for
// base64 again.
func TestEncoderFallsBackToFloatsWhenBase64Rejected(t *testing.T) {
	var base64Requests, floatRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req embeddingsRequest
		require.NoError(t, json.Unmarshal(body, &req))

		if req.EncodingFormat != "" {
			base64Requests.Add(1)
			writeJSON(t, w, http.StatusBadRequest, map[string]any{
				"error": "unknown field: encoding_format",
			})
			return
		}
		floatRequests.Add(1)
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2, 3}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:  srv.URL + "/v1",
		Model:     "test-model",
		Dimension: 3,
		Timeout:   5 * time.Second,
	})

	out, err := enc(context.Background(), []string{"hello"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, []float32{1, 2, 3}, out[0])

	_, err = enc(context.Background(), []string{"again"})
	require.NoError(t, err)

	assert.Equal(t, int32(1), base64Requests.Load(),
		"the rejection downgrades the encoder for its lifetime")
	assert.Equal(t, int32(2), floatRequests.Load())
}

// TestEncoderInputAffixesApplied asserts configured affixes surround every
// input in the request body without mutating the caller's text slice, while
// returned vectors still map back to the original texts by index.
func TestEncoderInputAffixesApplied(t *testing.T) {
	var gotReq embeddingsRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &gotReq))
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{
				{Index: 0, Embedding: []float32{1, 2, 3}},
				{Index: 1, Embedding: []float32{4, 5, 6}},
			},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:    srv.URL + "/v1",
		Model:       "test-model",
		Dimension:   3,
		Timeout:     5 * time.Second,
		MaxRetries:  1,
		InputPrefix: "title: none | text: ",
		InputSuffix: "<|endoftext|>",
	})

	texts := []string{"hello", "world"}
	out, err := enc(context.Background(), texts)
	require.NoError(t, err)

	assert.Equal(t, []string{
		"title: none | text: hello<|endoftext|>",
		"title: none | text: world<|endoftext|>",
	}, gotReq.Input)
	assert.Equal(t, []string{"hello", "world"}, texts,
		"applying affixes must not mutate caller input")
	require.Len(t, out, 2)
	assert.Equal(t, []float32{1, 2, 3}, out[0])
	assert.Equal(t, []float32{4, 5, 6}, out[1])
}

func TestEncoderAnonymousNoAuthHeader(t *testing.T) {
	var gotAuth string
	var authSet bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, authSet = r.Header["Authorization"]
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2, 3}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 3,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.NoError(t, err)
	assert.False(t, authSet)
	assert.Empty(t, gotAuth)
}

func TestEncoderDimensionMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 3,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "got")
	assert.Contains(t, err.Error(), "want")
	assert.Contains(t, err.Error(), "[vector.embeddings] dimension")
}

// TestEncoderRequestDimensionsSentWhenConfigured asserts an encoder with
// RequestDimensions set sends the configured Dimension as the
// OpenAI-compatible "dimensions" request field and accepts the reduced
// vectors an honoring server returns.
func TestEncoderRequestDimensionsSentWhenConfigured(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &gotBody))
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:          srv.URL + "/v1",
		Model:             "test-model",
		Dimension:         2,
		RequestDimensions: true,
		Timeout:           5 * time.Second,
		MaxRetries:        1,
	})

	out, err := enc(context.Background(), []string{"hello"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, []float32{1, 2}, out[0])

	require.Contains(t, gotBody, "dimensions")
	assert.EqualValues(t, 2, gotBody["dimensions"],
		"the configured dimension is the requested output length")
}

// TestEncoderOmitsDimensionsFieldByDefault asserts the wire format is
// unchanged for native-dimension configurations: without RequestDimensions
// the request body carries no "dimensions" key at all, so endpoints that
// reject unknown fields keep working.
func TestEncoderOmitsDimensionsFieldByDefault(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &gotBody))
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2, 3}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 1,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.NoError(t, err)
	assert.NotContains(t, gotBody, "dimensions")
}

// TestEncoderDimensionsRejectionFailsFastWithActionableError asserts a 4xx
// response naming the dimensions field aborts immediately — no retry, no
// silent downgrade that would change the embedding space — with an error
// that names the request_dimensions setting and still carries the
// HTTPStatusError for the build loop's permanence classification.
func TestEncoderDimensionsRejectionFailsFastWithActionableError(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeJSON(t, w, http.StatusBadRequest, map[string]any{
			"error": "this model does not support the dimensions parameter",
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:          srv.URL + "/v1",
		Model:             "test-model",
		Dimension:         2,
		RequestDimensions: true,
		Timeout:           5 * time.Second,
		MaxRetries:        3,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "request_dimensions",
		"the error names the setting to change")
	assert.Contains(t, err.Error(), "native output length")
	assert.Equal(t, int32(1), requests.Load(), "a 4xx rejection is not retried")

	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.Status)
	assert.False(t, statusErr.Permanent(),
		"a config-level rejection must abort the build, not skip-stamp documents")
}

// TestEncoderNoDimensionsHintWhenFieldNotRequested asserts the actionable
// hint is gated on having actually sent the field: with RequestDimensions
// unset, a 4xx body that happens to mention dimensions flows through the
// normal error path untouched.
func TestEncoderNoDimensionsHintWhenFieldNotRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusBadRequest, map[string]any{
			"error": "input exceeds supported dimensions",
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 1,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "request_dimensions")
}

// TestEncoderDimensionMismatchWhenEndpointIgnoresRequestedDimensions asserts
// a server that silently ignores the dimensions field and returns
// native-length vectors produces an error saying so, instead of the vectors
// being truncated client-side or the generic mismatch message leaving the
// user to guess.
func TestEncoderDimensionMismatchWhenEndpointIgnoresRequestedDimensions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2, 3, 4}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:          srv.URL + "/v1",
		Model:             "test-model",
		Dimension:         2,
		RequestDimensions: true,
		Timeout:           5 * time.Second,
		MaxRetries:        3,
	})

	out, err := enc(context.Background(), []string{"hello"})
	require.Error(t, err)
	assert.Nil(t, out, "wrong-length vectors are never truncated client-side")
	assert.Contains(t, err.Error(), "got 4, want 2")
	assert.Contains(t, err.Error(), "ignored the requested dimensions field")
	assert.Contains(t, err.Error(), "request_dimensions")
}

// TestEncoderBase64FallbackKeepsRequestedDimensions asserts the
// encoding_format float downgrade re-marshals the request with the
// dimensions field intact, so the transport fallback cannot silently change
// the requested output length.
func TestEncoderBase64FallbackKeepsRequestedDimensions(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(body, &decoded))
		bodies = append(bodies, decoded)

		if _, ok := decoded["encoding_format"]; ok {
			writeJSON(t, w, http.StatusBadRequest, map[string]any{
				"error": "unknown field: encoding_format",
			})
			return
		}
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:          srv.URL + "/v1",
		Model:             "test-model",
		Dimension:         2,
		RequestDimensions: true,
		Timeout:           5 * time.Second,
	})

	out, err := enc(context.Background(), []string{"hello"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, []float32{1, 2}, out[0])

	require.Len(t, bodies, 2, "base64 attempt plus the float retry")
	for i, b := range bodies {
		assert.EqualValues(t, 2, b["dimensions"],
			"request %d carries the requested dimensions", i)
	}
}

func TestEncoderCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2, 3}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 3,
	})

	_, err := enc(context.Background(), []string{"hello", "world"})
	require.Error(t, err)
}

func TestEncoderRetries429ThenSucceeds(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2, 3}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 3,
	})

	out, err := enc(context.Background(), []string{"hello"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, int32(2), attempts.Load())
}

func TestEncoderRetryRateLimitsSurvivesMoreRateLimitsThanMaxRetries(t *testing.T) {
	// Reproduces #1353: embeddings build must not abort permanently on a
	// 429 that clears with time. 5 consecutive 429s exceed MaxRetries: 1,
	// so without RetryRateLimits this would fail on the second attempt.
	var attempts atomic.Int32
	const rateLimitedResponses = 5

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= rateLimitedResponses {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2, 3}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:        srv.URL + "/v1",
		Model:           "test-model",
		Dimension:       3,
		Timeout:         5 * time.Second,
		MaxRetries:      1,
		RetryRateLimits: true,
	})

	out, err := enc(context.Background(), []string{"hello"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	// Not an exact count: Go's transport can silently retry a request once at
	// the connection level after a backoff-induced idle period, so the raw
	// request count the server observes isn't 1:1 with encode()'s own retry
	// counter. What actually matters is that MORE than MaxRetries rate-limited
	// responses didn't abort the call.
	assert.GreaterOrEqual(t, attempts.Load(), int32(rateLimitedResponses+1))
}

func TestEncoderRetryRateLimitsDisabledFailsFastOn429PastMaxRetries(t *testing.T) {
	// Without RetryRateLimits, a 429 is just another retryable error and
	// still respects MaxRetries -- this is the pre-existing behavior for
	// latency-sensitive query encoders (see newVectorQueryEncoder).
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 2,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.Error(t, err)
	assert.Equal(t, int32(2), attempts.Load())
}

func TestEncoder500ExhaustsRetries(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server error"))
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 3,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.Error(t, err)
	assert.Equal(t, int32(3), attempts.Load())
}

func TestEncoder400FailsWithoutRetry(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request: invalid input"))
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 3,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.Error(t, err)
	assert.Equal(t, int32(1), attempts.Load())
	assert.Contains(t, err.Error(), "400")
}

func TestEncoderContextCancellationAbortsBackoffPromptly(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint:   srv.URL + "/v1",
		Model:      "test-model",
		Dimension:  3,
		Timeout:    5 * time.Second,
		MaxRetries: 10,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := enc(ctx, []string{"hello"})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 2*time.Second, "backoff should abort promptly on context cancellation")
}

// TestEncoder400ReturnsPermanentHTTPStatusError covers fix 1a: a non-200
// response must come back as a *HTTPStatusError carrying the status code,
// so callers (kit's WithFillEncodeError handler) can distinguish a permanent
// rejection from a transient one instead of string-matching the message.
func TestEncoder400ReturnsPermanentHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("token window overflow"))
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: 5 * time.Second, MaxRetries: 1,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.Error(t, err)
	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.Status)
	assert.True(t, statusErr.Permanent(), "a 400 is a permanent rejection")
}

// TestEncoder429ReturnsNonPermanentHTTPStatusError guards the except-429
// carve-out: rate-limiting is a 4xx status but must not be treated as a
// permanent content rejection, or a poison-document skip would wrongly
// swallow a document that would have succeeded once the rate limit
// cleared.
func TestEncoder429ReturnsNonPermanentHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: 5 * time.Second, MaxRetries: 1,
	})

	_, err := enc(context.Background(), []string{"hello"})
	require.Error(t, err)
	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusTooManyRequests, statusErr.Status)
	assert.False(t, statusErr.Permanent(), "429 is transient rate-limiting, not a content rejection")
}

// TestHTTPStatusErrorPermanentClassification pins the skip-vs-abort
// classification: only input-specific rejections (400/413/422 whose body
// describes an input-size overflow or a content-policy refusal) may
// skip-stamp a poison document. Everything else — auth, routing, model,
// media-type, rate-limit, server errors, or an allowlisted status with a
// nonspecific body — must abort the build, or a config mistake would
// silently skip-stamp an entire corpus as embedded-with-no-vectors.
func TestHTTPStatusErrorPermanentClassification(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		permanent bool
	}{
		{http.StatusBadRequest, "token window overflow", true},
		{http.StatusBadRequest, "maximum context length is 8192 tokens", true},
		{http.StatusRequestEntityTooLarge, "input too large", true},
		{http.StatusUnprocessableEntity, "content policy violation", true},
		{http.StatusBadRequest, "", false},
		{http.StatusBadRequest, "no route", false},
		{http.StatusBadRequest, "invalid token", false},
		{http.StatusBadRequest, "invalid model", false},
		{http.StatusUnprocessableEntity, "unsupported content type", false},
		{http.StatusNotFound, "model not found", false},
		{http.StatusUnauthorized, "invalid token", false},
		{http.StatusForbidden, "forbidden", false},
		{http.StatusTooManyRequests, "input rate limited", false},
		{http.StatusInternalServerError, "token error", false},
		{http.StatusBadGateway, "", false},
	}
	for _, tc := range cases {
		err := &HTTPStatusError{Status: tc.status, Body: tc.body}
		assert.Equalf(t, tc.permanent, err.Permanent(),
			"status %d body %q: Permanent() classification", tc.status, tc.body)
	}
}

// TestEncoderDecodeErrorIsRetried covers fix 2: a decoding failure almost
// always means the connection died mid-stream, not that the endpoint sent
// a deliberately malformed response, so it must be retried rather than
// failing the whole encode on the first garbled response.
func TestEncoderDecodeErrorIsRetried(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			_, _ = w.Write([]byte("{not valid json"))
			return
		}
		require.NoError(t, json.MarshalWrite(w, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2, 3}}},
		}))
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: 5 * time.Second, MaxRetries: 3,
	})

	out, err := enc(context.Background(), []string{"hello"})
	require.NoError(t, err, "a truncated/garbled body must be retried, not fail outright")
	require.Len(t, out, 1)
	assert.Equal(t, int32(2), attempts.Load())
}

// --- fix 5: Retry-After ---

func TestParseRetryAfterDeltaSeconds(t *testing.T) {
	d, ok := parseRetryAfter("30", time.Now())
	require.True(t, ok)
	assert.Equal(t, 30*time.Second, d)
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	future := now.Add(45 * time.Second)
	d, ok := parseRetryAfter(future.Format(http.TimeFormat), now)
	require.True(t, ok)
	assert.InDelta(t, float64(45*time.Second), float64(d), float64(time.Second))
}

func TestParseRetryAfterZeroMeansImmediate(t *testing.T) {
	d, ok := parseRetryAfter("0", time.Now())
	require.True(t, ok)
	assert.Zero(t, d)
}

func TestParseRetryAfterAbsentOrUnparseableReturnsNotOK(t *testing.T) {
	_, ok := parseRetryAfter("", time.Now())
	assert.False(t, ok, "empty header")

	_, ok = parseRetryAfter("not-a-value", time.Now())
	assert.False(t, ok, "unparseable header")
}

func TestParseRetryAfterCappedAtSixtySeconds(t *testing.T) {
	d, ok := parseRetryAfter("3600", time.Now())
	require.True(t, ok)
	assert.Equal(t, 60*time.Second, d, "a huge Retry-After must be capped rather than honored verbatim")
}

func TestParseRetryAfterPastHTTPDateClampsToZero(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	d, ok := parseRetryAfter(past.Format(http.TimeFormat), now)
	require.True(t, ok)
	assert.Zero(t, d, "an already-past Retry-After date means retry immediately")
}

func TestBackoffDelayHonorsRetryAfterOn429(t *testing.T) {
	d := 3 * time.Second
	err := &HTTPStatusError{Status: http.StatusTooManyRequests, RetryAfter: &d}
	assert.Equal(t, 3*time.Second, backoffDelay(1, err))
}

func TestBackoffDelayFallsBackToExponentialWithoutRetryAfter(t *testing.T) {
	err := &HTTPStatusError{Status: http.StatusTooManyRequests}
	assert.Equal(t, backoffBase, backoffDelay(1, err))
	assert.Equal(t, 2*backoffBase, backoffDelay(2, err))
}

func TestBackoffDelayFallsBackForNonStatusErrors(t *testing.T) {
	assert.Equal(t, backoffBase, backoffDelay(1, errors.New("network error")))
}

// TestEncoderHonorsRetryAfterHeaderOn429 drives the full retry path end to
// end: a 429 response carrying Retry-After must make the encoder wait at
// least that long (not the default 250ms backoff) before its next attempt.
func TestEncoderHonorsRetryAfterHeaderOn429(t *testing.T) {
	var attempts atomic.Int32
	var firstAt, secondAt time.Time

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			firstAt = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		secondAt = time.Now()
		writeJSON(t, w, http.StatusOK, embeddingsResponse{
			Data: []embeddingDatum{{Index: 0, Embedding: []float32{1, 2, 3}}},
		})
	}))
	defer srv.Close()

	enc := NewEncoder(EncoderConfig{
		Endpoint: srv.URL + "/v1", Model: "test-model", Dimension: 3,
		Timeout: 5 * time.Second, MaxRetries: 2,
	})

	out, err := enc(context.Background(), []string{"hello"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.GreaterOrEqual(t, secondAt.Sub(firstAt), 900*time.Millisecond,
		"the encoder must wait out the server's Retry-After: 1 rather than the default ~250ms backoff")
}
