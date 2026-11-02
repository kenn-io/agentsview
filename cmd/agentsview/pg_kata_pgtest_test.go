//go:build pgtest

package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPGServeKataHubConnection(t *testing.T) {
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL must point to a dedicated test database")
	}

	// The nonexistent schema keeps the raw-sync privilege probe read-only and
	// avoids relying on the other PostgreSQL integration tests' shared schema.
	const schema = "kata_hub_probe_missing"
	store, err := postgres.NewStore(pgURL, schema, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	kataServer := katatest.New(t)
	cfg := config.Config{
		Host: "127.0.0.1",
		Port: 8080,
		PG:   config.PGConfig{URL: pgURL},
		Kata: config.KataConfig{
			Enabled: true, Endpoint: kataServer.Endpoint(), Project: "agentsview",
		},
	}
	opts, cleanup, err := (pgReplica{}).serveOptions(
		t.Context(), cfg, storage.ReplicaTarget{Schema: schema}, store,
	)
	require.NoError(t, err)
	if cleanup != nil {
		t.Cleanup(func() { require.NoError(t, cleanup()) })
	}
	srv := server.New(cfg, store, nil, opts...)

	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Host = "127.0.0.1:8080"
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	w := get("/api/v1/kata/status")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var status kata.Status
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &status))
	assert.Equal(t, kata.StateReady, status.State)
	assert.Equal(t, "agentsview", status.Project)

	w = get("/api/v1/version")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var version server.VersionInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &version))
	assert.True(t, version.KataAvailable)
	assert.NotEmpty(t, kataServer.RequestsMatching(http.MethodGet, "/api/v1/health"))
}
