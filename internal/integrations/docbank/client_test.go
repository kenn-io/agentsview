package docbank

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"hegel.dev/go/hegel"
)

func TestLookup(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	const version = "12345678-1234-1234-1234-123456789abc"
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/content-references", r.URL.Path)
		assert.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
		assert.Equal(t, "limit=10&offset=0&sha256="+digest, r.URL.RawQuery)
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"items":[],"total":0}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"version":{"id":"` + version + `","node_id":1},"node":{"id":1,"name":"current.png"},"path":"/images/current.png","is_current":true},{"version":{"id":"` + version + `","node_id":2},"node":{"id":2,"name":"prior.png"},"path":"/images/prior.png","is_current":false},{"version":{"id":"` + version + `","node_id":3},"node":{"id":3,"name":"trash.png","trashed_at":"2026-01-01T00:00:00Z"},"path":"/old/path","is_current":true}],"total":15}`))
	}))
	defer srv.Close()
	cfg := config.DocbankIntegration{URL: srv.URL, WebURL: "https://example.com/docbank/", APIKeyEnv: "DOCBANK_TEST_KEY"}
	result, err := New(cfg).Lookup(t.Context(), digest)
	require.NoError(t, err)
	assert.Zero(t, result.Total)
	assert.Empty(t, result.Matches)
	result, err = New(cfg).Lookup(t.Context(), digest)
	require.NoError(t, err)
	assert.Equal(t, 15, result.Total)
	require.Len(t, result.Matches, 3)
	assert.Equal(t, Match{State: "current", Name: "current.png", Path: "/images/current.png", OpenURL: "https://example.com/docbank/#node=1&version=" + version}, result.Matches[0])
	assert.Equal(t, "prior", result.Matches[1].State)
	assert.Equal(t, "trash", result.Matches[2].State)
	assert.Empty(t, result.Matches[2].Path)
	cfg.WebURL = ""
	result, err = New(cfg).Lookup(t.Context(), digest)
	require.NoError(t, err)
	assert.Empty(t, result.Matches[0].OpenURL)
}

func TestLookupCache(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	var requests atomic.Int32
	failNext := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if failNext {
			failNext = false
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer srv.Close()
	client := New(config.DocbankIntegration{URL: srv.URL, APIKeyEnv: "DOCBANK_TEST_KEY"})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return now }

	_, err := client.Lookup(t.Context(), digest)
	require.Error(t, err)
	_, err = client.Lookup(t.Context(), digest)
	require.NoError(t, err, "a failure is not cached")
	_, err = client.Lookup(t.Context(), digest)
	require.NoError(t, err)
	assert.Equal(t, int32(2), requests.Load(), "a success is reused within the TTL")

	now = now.Add(cacheTTL)
	_, err = client.Lookup(t.Context(), digest)
	require.NoError(t, err)
	assert.Equal(t, int32(3), requests.Load(), "an expired answer is fetched again")
}

func TestLookupFailures(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	for _, tc := range []struct {
		name, key, cause string
		handler          http.HandlerFunc
		unreachable      bool
	}{
		{"empty page", "secret-value", "", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }, false},
		{"status", "secret-value", "server returned HTTP 503", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, false},
		{"oversized", "secret-value", "", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"items":[],"total":0}` + strings.Repeat(" ", maxPageBytes+1)))
		}, false},
		{"redirect", "secret-value", "server returned HTTP 302", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }, false},
		{"missing key", "", "missing API key", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }, false},
		{"rejected key", "secret-value", "API key rejected", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, false},
		{"forbidden key", "secret-value", "API key rejected", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }, false},
		{"unreachable server", "secret-value", "server unreachable", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			t.Setenv("DOCBANK_TEST_KEY", tc.key)
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			if tc.unreachable {
				srv.Close()
			}
			_, err := New(config.DocbankIntegration{URL: srv.URL, APIKeyEnv: "DOCBANK_TEST_KEY"}).Lookup(t.Context(), strings.Repeat("a", 64))
			require.EqualError(t, err, "docbank unavailable")
			if tc.cause != "" {
				assert.Contains(t, logs.String(), tc.cause)
			}
			assert.NotContains(t, logs.String(), "secret-value")
		})
	}
	assert.Zero(t, hits.Load())
}

// Dial the configured container name only to a fixture on loopback.
func docbankPrivateHTTPTransport(t *testing.T, address string) *http.Transport {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "docbank.test:8485" {
			return nil, fmt.Errorf("unexpected fixture address %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

func TestLookupPrivateHTTPPreservesRequestAndBrowserURLs(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "synthetic-header-key")
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	const version = "12345678-1234-1234-1234-123456789abc"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		assert.Equal(t, "docbank.test:8485", r.Host)
		assert.Equal(t, "/a%20b/api/v1/content-references?limit=10&offset=0&sha256="+digest, r.RequestURI)
		assert.Nil(t, r.URL.User)
		assert.NotContains(t, r.RequestURI, "synthetic-header-key")
		assert.Equal(t, "synthetic-header-key", r.Header.Get("X-Api-Key"))
		_, _ = w.Write([]byte(`{"items":[{"version":{"id":"` + version + `"},"node":{"id":7,"name":"image.png"},"path":"/image.png","is_current":true}],"total":1}`))
	}))
	defer srv.Close()
	cfg := config.DocbankIntegration{URL: "http://docbank.test:8485/a%20b/", WebURL: "https://UI.EXAMPLE.test:443/images%20here/", APIKeyEnv: "DOCBANK_TEST_KEY", AllowHTTP: true}
	require.NoError(t, cfg.Validate())
	client := New(cfg)
	client.http.Transport = docbankPrivateHTTPTransport(t, srv.Listener.Addr().String())
	result, err := client.Lookup(t.Context(), digest)
	require.NoError(t, err)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "https://UI.EXAMPLE.test:443/images%20here/#node=7&version="+version, result.Matches[0].OpenURL)
	assert.Equal(t, int32(1), hits.Load())
}

func TestLookupPrivateHTTPRefusesEveryRedirect(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "synthetic-header-key")
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, sameOrigin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/same_origin=%t", status, sameOrigin), func(t *testing.T) {
				var initialHits, redirectHits atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectHits.Add(1) }))
				defer target.Close()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirected" {
						redirectHits.Add(1)
						return
					}
					initialHits.Add(1)
					assert.Equal(t, "synthetic-header-key", r.Header.Get("X-Api-Key"))
					location := target.URL
					if sameOrigin {
						location = "/redirected"
					}
					http.Redirect(w, r, location, status)
				}))
				defer srv.Close()
				cfg := config.DocbankIntegration{URL: "http://docbank.test:8485", APIKeyEnv: "DOCBANK_TEST_KEY", AllowHTTP: true}
				require.NoError(t, cfg.Validate())
				client := New(cfg)
				transport := docbankPrivateHTTPTransport(t, srv.Listener.Addr().String())
				privateDial := transport.DialContext
				transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					if addr == target.Listener.Addr().String() {
						return (&net.Dialer{}).DialContext(ctx, network, addr)
					}
					return privateDial(ctx, network, addr)
				}
				client.http.Transport = transport
				_, err := client.Lookup(t.Context(), strings.Repeat("a", 64))
				require.EqualError(t, err, "docbank unavailable")
				assert.Equal(t, int32(1), initialHits.Load())
				assert.Zero(t, redirectHits.Load())
			})
		}
	}
}

func TestLookupBasePathsPreserveMeaning(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "synthetic-header-key")
	type request struct{ uri, key string }
	requests := make(chan request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- request{r.RequestURI, r.Header.Get("X-Api-Key")}
		_, _ = w.Write([]byte(`{"items":[{"version":{"id":"12345678-1234-1234-1234-123456789abc"},"node":{"id":7,"name":"image.png"},"path":"/image.png","is_current":true}],"total":1}`))
	}))
	defer srv.Close()
	hegel.Test(t, func(ht *hegel.T) {
		apiID := hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))
		webID := hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))
		apiPath := fmt.Sprintf("/api-%x/a%%20b/", apiID)
		webPath := fmt.Sprintf("https://browser.example.test/ui-%x/a%%2Fb/", webID)
		cfg := config.DocbankIntegration{URL: srv.URL + apiPath, WebURL: webPath, APIKeyEnv: "DOCBANK_TEST_KEY", AllowHTTP: hegel.Draw(ht, hegel.Booleans())}
		require.NoError(ht, cfg.Validate())
		result, err := New(cfg).Lookup(t.Context(), strings.Repeat("a", 64))
		require.NoError(ht, err)
		got := <-requests
		assert.Equal(ht, fmt.Sprintf("/api-%x/a%%20b/api/v1/content-references?limit=10&offset=0&sha256=", apiID)+strings.Repeat("a", 64), got.uri)
		assert.Equal(ht, "synthetic-header-key", got.key)
		require.Len(ht, result.Matches, 1)
		assert.Equal(ht, fmt.Sprintf("https://browser.example.test/ui-%x/a%%2Fb/#node=7&version=12345678-1234-1234-1234-123456789abc", webID), result.Matches[0].OpenURL)
	}, hegel.WithTestCases(100))
}
