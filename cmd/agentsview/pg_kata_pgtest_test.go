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
	"go.kenn.io/agentsview/internal/service"
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
	store, err := postgres.NewStore(t.Context(), pgURL, schema, false)
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

func TestPGServeUsageAfterCacheSetupFailure(t *testing.T) {
	pg, directory, url := newRawSyncCleanUploadsPG(t)
	ctx := t.Context()
	_, err := pg.ExecContext(ctx, `INSERT INTO cursor_usage_events(id,agent,occurred_at,model,output_tokens,charged_microdollars,dedup_key) VALUES (1,'cursor','2026-01-01','model-a',7,77,'startup-account')`)
	require.NoError(t, err)
	_, err = pg.ExecContext(ctx, `UPDATE sync_metadata SET value='broken' WHERE key='usage_change_revision'`)
	require.NoError(t, err)
	store, err := postgres.NewStore(ctx, url, rawSyncCleanUploadsSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	require.Error(t, store.ConfigureUsageCache(ctx, directory, url, rawSyncCleanUploadsSchema))
	cfg := config.Config{DataDir: directory, Host: "127.0.0.1", Port: 8080, PG: config.PGConfig{URL: url}}
	opts, cleanup, err := (pgReplica{}).serveOptions(ctx, cfg, storage.ReplicaTarget{URL: url, Schema: rawSyncCleanUploadsSchema}, store)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })
	srv := server.New(cfg, store, nil, opts...)
	response := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/usage/summary?from=2026-01-01&to=2026-01-01&timezone=UTC", nil)
	request.Host = "127.0.0.1:8080"
	srv.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result service.UsageSummaryResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.Equal(t, 7, result.Totals.OutputTokens)
	require.Equal(t, int64(77), result.Totals.TotalCost.Microdollars)
}
