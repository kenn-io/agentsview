package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/postgres"
	"hegel.dev/go/hegel"
)

func TestHostedRawHTTPWorkRefused(t *testing.T) {
	f := newSyncRouteFixture(t)
	cfg := f.srv.cfg
	cfg.RawSync.Hosted = true
	srv := New(cfg, f.db, nil, WithReplicas(postgres.Backend{}, clickhouse.Backend{}), WithMirror(duckdb.Mirror{}))

	handler := srv.Handler()
	t.Run("sync-regression", func(t *testing.T) {
		response := serveJSON(t, handler, http.MethodPost, "/api/v1/sync", nil)
		require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), "[raw_sync] hosted")
	})
	hegel.Test(t, func(ht *hegel.T) {
		path := hegel.Draw(ht, hegel.SampledFrom([]string{"/api/v1/sync", "/api/v1/sync/remotes", "/api/v1/sessions/sync", "/api/v1/push/pg", "/api/v1/push/clickhouse", "/api/v1/push/duckdb", "/api/v1/embeddings/build"}))
		var body any = map[string]any{"id": "fixture"}
		switch path {
		case "/api/v1/sync/remotes":
			body = remoteSyncRequest{}
		case "/api/v1/push/pg", "/api/v1/push/clickhouse", "/api/v1/push/duckdb":
			body = daemonPushRequest{}
		case "/api/v1/embeddings/build":
			body = embeddingsBuildRequest{}
		}
		response := serveJSON(t, handler, http.MethodPost, path, body)
		require.Equal(ht, http.StatusForbidden, response.Code, response.Body.String())
		require.Contains(ht, response.Body.String(), "[raw_sync] hosted")
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
	require.Nil(t, srv.onDemandEngine)
}
