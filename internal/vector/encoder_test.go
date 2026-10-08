package vector

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
)

// embeddingsRequest captures the OpenAI-compatible request body the encoder
// sends, keeping optional fields raw so tests can assert their absence.
type embeddingsRequest struct {
	Model          string         `json:"model"`
	Input          []string       `json:"input"`
	Dimensions     jsontext.Value `json:"dimensions"`
	EncodingFormat jsontext.Value `json:"encoding_format"`
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.MarshalWrite(w, v))
}

// vectorsResponse answers every input with vector, by index.
func vectorsResponse(count int, vector []float32) map[string]any {
	data := make([]map[string]any, count)
	for i := range data {
		data[i] = map[string]any{"index": i, "embedding": vector}
	}
	return map[string]any{"data": data}
}

func testEncoderConfig(endpoint string) EncoderConfig {
	return EncoderConfig{
		Model: embedconfig.Model{
			Name: "test-model", Dimensions: 3,
			Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone,
		},
		Deployment: embedconfig.Deployment{BaseURL: endpoint},
		Transport:  embedconfig.Transport{Timeout: 5 * time.Second},
		MaxRetries: 3,
	}
}

func TestEncoderAppliesRoleAffixesAndKeepsEndpointValues(t *testing.T) {
	var gotPath, gotAuth string
	var gotReq embeddingsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &gotReq)) {
			return
		}
		writeJSON(t, w, http.StatusOK, vectorsResponse(len(gotReq.Input), []float32{3, 4, 0}))
	}))
	defer srv.Close()

	cfg := testEncoderConfig(srv.URL + "/v1")
	cfg.APIKey = "secret-key"
	cfg.Roles = embedconfig.Roles{
		DocumentPrefix: "doc: ", DocumentSuffix: "<|end|>",
		QueryPrefix: "query: ", QuerySuffix: "<|end|>",
	}
	documents, err := NewEncoder(cfg, embedconfig.RoleDocument)
	require.NoError(t, err)
	queries, err := NewEncoder(cfg, embedconfig.RoleQuery)
	require.NoError(t, err)

	out, err := documents(t.Context(), []string{"alpha", "beta"})
	require.NoError(t, err)
	assert.Equal(t, "/v1/embeddings", gotPath)
	assert.Equal(t, "Bearer secret-key", gotAuth)
	assert.Equal(t, "test-model", gotReq.Model)
	assert.Equal(t, []string{"doc: alpha<|end|>", "doc: beta<|end|>"}, gotReq.Input)
	assert.Nil(t, gotReq.Dimensions, "dimensions is omitted unless requested")
	assert.Nil(t, gotReq.EncodingFormat, "the default float wire format is used")
	assert.Equal(t, [][]float32{{3, 4, 0}, {3, 4, 0}}, out,
		"vectors are stored as the endpoint returned them, unnormalized")

	_, err = queries(t.Context(), []string{"gamma"})
	require.NoError(t, err)
	assert.Equal(t, []string{"query: gamma<|end|>"}, gotReq.Input)
}

func TestEncoderRequestsDimensionsWhenConfigured(t *testing.T) {
	var gotReq embeddingsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &gotReq)) {
			return
		}
		writeJSON(t, w, http.StatusOK, vectorsResponse(len(gotReq.Input), []float32{1, 0, 0}))
	}))
	defer srv.Close()

	cfg := testEncoderConfig(srv.URL + "/v1")
	cfg.Model.RequestDimensions = true
	enc, err := NewEncoder(cfg, embedconfig.RoleDocument)
	require.NoError(t, err)

	_, err = enc(t.Context(), []string{"alpha"})
	require.NoError(t, err)
	assert.JSONEq(t, "3", string(gotReq.Dimensions))
}

// base64Vector encodes components as the little-endian float32 payload of
// the OpenAI base64 embedding format.
func base64Vector(components ...float32) string {
	data := make([]byte, 4*len(components))
	for i, c := range components {
		binary.LittleEndian.PutUint32(data[4*i:], math.Float32bits(c))
	}
	return base64.StdEncoding.EncodeToString(data)
}

func TestEncoderRejectsInvalidVectors(t *testing.T) {
	nan := float32(math.NaN())
	inf := float32(math.Inf(1))
	for _, tt := range []struct {
		name      string
		embedding any
		err       string
	}{
		{"short", []float32{1, 0}, "2 dimensions"},
		{"oversized", []float32{1, 0, 0, 0}, "4 dimensions"},
		{"zero", []float32{0, 0, 0}, "zero norm"},
		{"float32 overflow", []float64{1e39, 1, 0}, "not finite"},
		{"base64 NaN", base64Vector(nan, 1, 0), "not finite"},
		{"base64 infinity", base64Vector(inf, 1, 0), "not finite"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, map[string]any{"data": []map[string]any{
					{"index": 0, "embedding": tt.embedding},
				}})
			}))
			defer srv.Close()

			cfg := testEncoderConfig(srv.URL + "/v1")
			cfg.MaxRetries = 0
			enc, err := NewEncoder(cfg, embedconfig.RoleDocument)
			require.NoError(t, err)

			out, err := enc(t.Context(), []string{"alpha"})
			require.ErrorContains(t, err, tt.err)
			assert.Nil(t, out, "invalid output must not leave usable vectors")
		})
	}
}

// rateLimitedServer answers 429 for the first limited calls, then succeeds.
func rateLimitedServer(t *testing.T, limited int32, retryAfter string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingsRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
			return
		}
		if calls.Add(1) <= limited {
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(t, w, http.StatusOK, vectorsResponse(len(req.Input), []float32{1, 0, 0}))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestEncoderRetryRateLimitsOutlastsMaxRetries(t *testing.T) {
	srv, calls := rateLimitedServer(t, 3, "0")
	cfg := testEncoderConfig(srv.URL + "/v1")
	cfg.MaxRetries = 2
	cfg.RetryRateLimits = true
	enc, err := NewEncoder(cfg, embedconfig.RoleDocument)
	require.NoError(t, err)

	out, err := enc(t.Context(), []string{"alpha"})
	require.NoError(t, err, "a document build waits out rate limits past max_retries")
	assert.Equal(t, [][]float32{{1, 0, 0}}, out)
	assert.Equal(t, int32(4), calls.Load())
}

func TestEncoderRetryRateLimitsKeepsServerFailureBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1)%2 == 0 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := testEncoderConfig(srv.URL + "/v1")
	cfg.MaxRetries = 2
	cfg.RetryRateLimits = true
	enc, err := NewEncoder(cfg, embedconfig.RoleDocument)
	require.NoError(t, err)

	_, err = enc(t.Context(), []string{"alpha"})
	apiErr, ok := errors.AsType[*embedclient.APIError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	assert.Equal(t, int32(3), calls.Load(),
		"rate limits between server failures must not reset the max_retries budget")
}

func TestEncoderRateLimitFailsAfterMaxRetriesWithoutRetryRateLimits(t *testing.T) {
	srv, calls := rateLimitedServer(t, 100, "0")
	cfg := testEncoderConfig(srv.URL + "/v1")
	cfg.MaxRetries = 2
	enc, err := NewEncoder(cfg, embedconfig.RoleQuery)
	require.NoError(t, err)

	_, err = enc(t.Context(), []string{"alpha"})
	apiErr, ok := errors.AsType[*embedclient.APIError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
	assert.Equal(t, int32(2), calls.Load(), "query encoders spend only max_retries attempts")
}

func TestEncoderRateLimitWaitHonorsCancellation(t *testing.T) {
	srv, calls := rateLimitedServer(t, 100, "30")
	cfg := testEncoderConfig(srv.URL + "/v1")
	cfg.MaxRetries = 1
	cfg.RetryRateLimits = true
	enc, err := NewEncoder(cfg, embedconfig.RoleDocument)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := enc(ctx, []string{"alpha"})
		done <- err
	}()
	require.Eventually(t, func() bool { return calls.Load() >= 1 }, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		require.Fail(t, "encoder kept waiting out Retry-After after cancellation")
	}
}

func TestEncoderInputRejectionIsPermanentAndNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(t, w, http.StatusBadRequest, map[string]any{
			"error": "the input length exceeds the context length",
		})
	}))
	defer srv.Close()

	enc, err := NewEncoder(testEncoderConfig(srv.URL+"/v1"), embedconfig.RoleDocument)
	require.NoError(t, err)

	_, err = enc(t.Context(), []string{"alpha"})
	require.Error(t, err)
	assert.True(t, isPermanentEncodeError(err), "a 400 skips only the rejected document")
	assert.Equal(t, int32(1), calls.Load(), "an input rejection is never retried")
}

func TestEncoderServerErrorsSpendMaxRetriesAndStayRetryable(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := testEncoderConfig(srv.URL + "/v1")
	cfg.MaxRetries = 2
	enc, err := NewEncoder(cfg, embedconfig.RoleDocument)
	require.NoError(t, err)

	_, err = enc(t.Context(), []string{"alpha"})
	require.Error(t, err)
	assert.False(t, isPermanentEncodeError(err), "a server failure aborts the fill for a later retry")
	assert.Equal(t, int32(2), calls.Load())
}

func TestEncoderCredentialRejectionIsNotPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	enc, err := NewEncoder(testEncoderConfig(srv.URL+"/v1"), embedconfig.RoleDocument)
	require.NoError(t, err)

	_, err = enc(t.Context(), []string{"alpha"})
	require.Error(t, err)
	assert.False(t, isPermanentEncodeError(err),
		"a rejected key must abort the build instead of skipping every document")
}
