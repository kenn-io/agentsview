package vector

import (
	"context"
	"encoding/json"
	"io"
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
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embeddingDatum mirrors one element of the OpenAI-compatible response.
type embeddingDatum struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embeddingsResponse struct {
	Data []embeddingDatum `json:"data"`
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(v))
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
