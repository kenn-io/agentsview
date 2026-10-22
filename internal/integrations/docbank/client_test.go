package docbank

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }},
		{"oversized", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat(" ", maxPageBytes+1)))
		}},
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			_, err := New(config.DocbankIntegration{URL: srv.URL, APIKeyEnv: "DOCBANK_TEST_KEY"}).Lookup(t.Context(), strings.Repeat("a", 64))
			assert.EqualError(t, err, "docbank unavailable")
		})
	}
	assert.Zero(t, hits.Load())
}
