package docbank

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestLookup(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	const version = "12345678-1234-1234-1234-123456789abc"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/content-references", r.URL.Path)
		assert.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
		assert.Equal(t, "limit=10&offset=0&sha256="+digest, r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"items":[{"version":{"id":"` + version + `","node_id":1},"node":{"id":1,"name":"current.png"},"path":"/images/current.png","is_current":true},{"version":{"id":"` + version + `","node_id":2},"node":{"id":2,"name":"prior.png"},"path":"/images/prior.png","is_current":false},{"version":{"id":"` + version + `","node_id":3},"node":{"id":3,"name":"trash.png","trashed_at":"2026-01-01T00:00:00Z"},"path":"/old/path","is_current":true}],"total":15}`))
	}))
	defer srv.Close()
	cfg := config.DocbankIntegration{URL: srv.URL, WebURL: "https://example.com/docbank/", APIKeyEnv: "DOCBANK_TEST_KEY"}
	result, err := New(cfg).Lookup(t.Context(), digest)
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

type lookupTransport func(*http.Request) (*http.Response, error)

func (f lookupTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLookupCacheExpiryAndRetry(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "key")
	synctest.Test(t, func(t *testing.T) {
		hits := 0
		client := New(config.DocbankIntegration{URL: "https://example.com", APIKeyEnv: "DOCBANK_TEST_KEY"})
		client.http.Transport = lookupTransport(func(r *http.Request) (*http.Response, error) {
			hits++
			status := http.StatusOK
			if hits == 1 {
				status = http.StatusServiceUnavailable
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"items":[],"total":0}`)), Header: make(http.Header)}, nil
		})
		_, err := client.Lookup(t.Context(), "digest")
		require.Error(t, err)
		_, err = client.Lookup(t.Context(), "digest")
		require.NoError(t, err)
		assert.Equal(t, 2, hits)
		for _, digest := range []string{"second-digest", "digest", "second-digest"} {
			_, err = client.Lookup(t.Context(), digest)
			require.NoError(t, err)
		}
		assert.Equal(t, 3, hits)
		time.Sleep(59 * time.Second)
		_, err = client.Lookup(t.Context(), "digest")
		require.NoError(t, err)
		assert.Equal(t, 3, hits)
		time.Sleep(2 * time.Second)
		_, err = client.Lookup(t.Context(), "digest")
		require.NoError(t, err)
		assert.Equal(t, 4, hits)
	})
}
