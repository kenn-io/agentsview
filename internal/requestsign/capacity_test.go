package requestsign

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifierBoundsConcurrentHandlersUntilTheyReturn(t *testing.T) {
	const handlerLimit = 32

	key := signingKey(t)
	state := filepath.Join(t.TempDir(), "replay.db")
	require.NoError(t, InitReplay(state))
	replay, err := OpenReplay(state)
	require.NoError(t, err)
	t.Cleanup(func() { _ = replay.Close() })

	entered := make(chan struct{}, handlerLimit+1)
	release := make(chan struct{})
	released := false
	releaseAll := func() {
		if !released {
			close(release)
			released = true
		}
	}
	var handler http.Handler
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	verifier, err := NewVerifier(VerifyConfig{
		ExternalURL: ts.URL,
		Replay:      replay,
		Keys:        func() (map[string]Key, error) { return map[string]Key{key.ID: key}, nil },
	})
	require.NoError(t, err)
	handler = verifier.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}), nil, nil)
	signer, err := NewSigner(ts.URL, key)
	require.NoError(t, err)
	client, err := signer.Client(ts.Client())
	require.NoError(t, err)

	defer releaseAll()
	responses := make(chan *http.Response, handlerLimit)
	requestErrors := make(chan error, handlerLimit)
	for range handlerLimit {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/version", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer test")
		go func() {
			resp, err := client.Do(req) //nolint:bodyclose // Responses close after their handlers release below.
			if err != nil {
				requestErrors <- err
				return
			}
			responses <- resp
		}()
		select {
		case <-entered:
		case err := <-requestErrors:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			require.FailNow(t, "all admitted requests should reach the handler")
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/version", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test")
	resp, err := client.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	releaseAll()
	for range handlerLimit {
		select {
		case resp := <-responses:
			assert.Equal(t, http.StatusNoContent, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
		case err := <-requestErrors:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			require.FailNow(t, "released handlers should finish")
		}
	}

	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/version", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test")
	resp, err = client.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
}
