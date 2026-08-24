package rawclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, baseURL string, margin time.Duration) *Client {
	t.Helper()
	client, err := NewClient(Config{
		BaseURL: baseURL, DeviceID: "dev_test",
		Credential: "avdc_test", TokenMargin: margin,
	})
	require.NoError(t, err)
	return client
}

// newTokenTestServer mounts only the token-exchange route. It counts
// exchanges and issues tokens with the given TTL.
func newTokenTestServer(t *testing.T, exchanges *int32, ttl time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler goroutines use assert, never require/FailNow; the
		// response is always written so a mismatch fails fast, not hangs.
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/raw-sync/tokens", r.URL.Path)
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Equal(t, `{"scopes":["negotiate","upload","commit"]}`, string(body))
		assert.Equal(t, "Bearer avdc_test", r.Header.Get("Authorization"))
		assert.Equal(t, "dev_test", r.Header.Get("X-AgentsView-Device-ID"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":"avdt_%d","device_id":"dev_test",`+
			`"scopes":["negotiate","upload","commit"],`+
			`"expires_at":%q}`, atomic.AddInt32(exchanges, 1),
			time.Now().Add(ttl).UTC().Format(time.RFC3339Nano))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestTokenProviderCachesUntilExpiryMargin(t *testing.T) {
	t.Parallel()
	var exchanges int32
	server := newTokenTestServer(t, &exchanges, 10*time.Minute)
	client := newTestClient(t, server.URL, time.Minute)

	first, err := client.tokens.token(t.Context())
	require.NoError(t, err)
	require.Equal(t, "avdt_1", first)

	second, err := client.tokens.token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "avdt_1", second)
	assert.EqualValues(t, 1, atomic.LoadInt32(&exchanges))
}

func TestTokenProviderRefreshesPastMargin(t *testing.T) {
	t.Parallel()
	var exchanges int32
	// TTL below the margin, so the issued token is already stale for this
	// client the moment it is cached and the second call must re-exchange.
	server := newTokenTestServer(t, &exchanges, time.Minute)
	client := newTestClient(t, server.URL, 90*time.Second)

	_, err := client.tokens.token(t.Context())
	require.NoError(t, err)
	second, err := client.tokens.token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "avdt_2", second)
	assert.EqualValues(t, 2, atomic.LoadInt32(&exchanges))
}

func TestTokenProviderSingleFlight(t *testing.T) {
	t.Parallel()
	var exchanges int32
	server := newTokenTestServer(t, &exchanges, 10*time.Minute)
	client := newTestClient(t, server.URL, time.Minute)

	const callers = 8
	results := make(chan string, callers)
	for range callers {
		go func() {
			// assert, not require: the send must be unconditional so the
			// result loop below always drains.
			token, err := client.tokens.token(context.Background())
			assert.NoError(t, err)
			results <- token
		}()
	}
	for range callers {
		assert.NotEmpty(t, <-results)
	}
	assert.EqualValues(t, 1, atomic.LoadInt32(&exchanges))
}

// TestDoRetriesWithRefreshedTokenAfterUnauthorized covers do's forced-refresh
// path: a 401 on a data route invalidates the cached token, triggers one
// re-exchange, and the retried call succeeds with the new token.
func TestDoRetriesWithRefreshedTokenAfterUnauthorized(t *testing.T) {
	t.Parallel()
	var exchanges, objectCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// assert (not require): this handler runs on a server goroutine.
		switch r.URL.Path {
		case "/api/v1/raw-sync/tokens":
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "Bearer avdc_test", r.Header.Get("Authorization"))
			assert.Equal(t, "dev_test", r.Header.Get("X-AgentsView-Device-ID"))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"token":"avdt_%d","device_id":"dev_test",`+
				`"scopes":["negotiate","upload","commit"],`+
				`"expires_at":%q}`, atomic.AddInt32(&exchanges, 1),
				time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339Nano))
		case "/api/v1/raw-sync/objects/missing":
			assert.Equal(t, http.MethodGet, r.Method)
			w.Header().Set("Content-Type", "application/json")
			if atomic.AddInt32(&objectCalls, 1) == 1 {
				assert.Equal(t, "Bearer avdt_1", r.Header.Get("Authorization"))
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"code":"unauthorized","error":"Unauthorized"}`)
				return
			}
			assert.Equal(t, "Bearer avdt_2", r.Header.Get("Authorization"))
			fmt.Fprint(w, `{"stored":true}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL, time.Minute)

	resp, err := client.do(t.Context(), http.MethodGet, "/api/v1/raw-sync/objects/missing", nil, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.EqualValues(t, 2, atomic.LoadInt32(&exchanges))
	assert.EqualValues(t, 2, atomic.LoadInt32(&objectCalls))
}
