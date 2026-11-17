//go:build pgtest

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestHostedSemanticUnavailableGuidance(t *testing.T) {
	cfg, _ := hostedRuntimeConfig(t)
	store, err := postgres.NewHostedStore(t.Context(), cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	defaults, err := config.Default()
	require.NoError(t, err)
	defaults.Vector.Embeddings.Model = "fixture"
	defaults.Vector.Embeddings.Dimension = 2
	defaults.Vector.Embeddings.DefaultServer = "fixture"
	defaults.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"fixture": {Endpoint: "http://example.test", Timeout: "30s"}}
	for _, tc := range []struct {
		name    string
		vector  config.VectorConfig
		server  *config.VectorEmbeddingsServerConfig
		enabled bool
		want    string
	}{
		{name: "disabled", want: "semantic search: configure [vector]"},
		{name: "missing generation", vector: defaults.Vector, enabled: true, want: "generation not published"},
		{name: "missing deployment recipe", server: &config.VectorEmbeddingsServerConfig{Endpoint: "http://example.test"}, want: "semantic recipe adoption requires exactly one supported published generation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.Vector = tc.vector
			cfg.Vector.Enabled = tc.enabled
			cfg.DeploymentEmbeddings = tc.server
			require.NoError(t, wireReplicaVectorSearch(t.Context(), cfg, &serveWatchVectorProvider{miss: true}, store, "hosted"))
			_, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "hello", Mode: "semantic"})
			require.ErrorIs(t, err, db.ErrSemanticUnavailable)
			assert.Contains(t, err.Error(), tc.want)
			if tc.enabled || tc.server != nil {
				assert.Contains(t, err.Error(), "hosted-provision")
			}
			assert.NotContains(t, err.Error(), "replica pg push")
			assert.NotContains(t, err.Error(), "configured workstation")
		})
	}
}

func TestHostedDeploymentEmbeddingsSearch(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	defaults, err := config.Default()
	require.NoError(t, err)
	cfg.Vector = defaults.Vector
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0}}}, "model": "fixture", "object": "list"})
	}))
	t.Cleanup(endpoint.Close)
	cfg.Vector.Embeddings.Model = "fixture"
	cfg.Vector.Embeddings.Dimension = 2
	cfg.Vector.Embeddings.MaxInputChars = 128
	cfg.Vector.Embeddings.DefaultServer = "fixture"
	cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"fixture": {Endpoint: endpoint.URL, Timeout: "30s"}}
	gen := vectorGeneration(cfg.Vector.Embeddings)
	info := storage.VectorGenerationInfo{Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: gen.Dimensions, Params: gen.Params}
	_, err = admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	require.NoError(t, postgres.ProvisionHostedVector(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, info))
	id, _, ok, err := postgres.LookupVectorGeneration(t.Context(), admin, info.Fingerprint)
	require.NoError(t, err)
	require.True(t, ok)
	u, err := url.Parse(cfg.PG.URL)
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), fmt.Sprintf(`GRANT SELECT ON vector_chunks_g%d TO %q`, id, u.User.Username()))
	require.NoError(t, err)
	cfg.DeploymentEmbeddings = &config.VectorEmbeddingsServerConfig{Endpoint: endpoint.URL, BatchSize: 32, Concurrency: 4, Timeout: "30s"}
	cfg.Vector.Enabled = false
	startup, err := prepareReplicaServeImpl(t.Context(), pgReplica{}, cfg, "", nil, false)
	require.NoError(t, err)
	defer startup.cleanup()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/search/content?pattern=hello&mode=semantic&include_one_shot=true", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("X-AgentsView-Search-Intent", "semantic")
	response := httptest.NewRecorder()
	startup.srv.Handler().ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.EqualValues(t, 1, calls.Load())
}

func TestHostedCaptureSemanticSearch(t *testing.T) {
	original := newHostedRawParser
	newHostedRawParser = func(time.Duration) (hostedRawParser, error) {
		p, err := rawderive.NewProviderParser(parser.ProviderFactories(), "hosted")
		return testHostedRawParser{p}, err
	}
	t.Cleanup(func() { newHostedRawParser = original })
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "fixture", "object": "list"})
	}))
	t.Cleanup(endpoint.Close)
	var configured config.Config
	startup, store, admin := startParityRuntimeConfigured(t, config.ArchiveContentFull, false, func(cfg *config.Config, admin *sql.DB) {
		defaults, err := config.Default()
		require.NoError(t, err)
		cfg.Vector = defaults.Vector
		cfg.Vector.Enabled = true
		cfg.Vector.Embeddings.Model = "fixture"
		cfg.Vector.Embeddings.Dimension = 2
		cfg.Vector.Embeddings.MaxInputChars = 128
		cfg.Vector.Embeddings.DefaultServer = "fixture"
		cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"fixture": {Endpoint: endpoint.URL, BatchSize: 32, Timeout: "30s"}}
		_, err = admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
		require.NoError(t, err)
		gen := vectorGeneration(cfg.Vector.Embeddings)
		info := storage.VectorGenerationInfo{Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: gen.Dimensions, Params: gen.Params}
		require.NoError(t, postgres.ProvisionHostedVector(t.Context(), admin, cfg.PG.Schema, cfg.PG.RawTenant, info))
		id, _, ok, err := postgres.LookupVectorGeneration(t.Context(), admin, info.Fingerprint)
		require.NoError(t, err)
		require.True(t, ok)
		u, err := url.Parse(cfg.PG.URL)
		require.NoError(t, err)
		_, err = admin.ExecContext(t.Context(), fmt.Sprintf(`GRANT SELECT,INSERT,DELETE ON vector_chunks_g%d TO %q`, id, u.User.Username()))
		require.NoError(t, err)
		configured = *cfg
	})
	require.NoError(t, wireReplicaVectorSearch(t.Context(), configured, pgReplica{}, store, "hosted test"))
	root := t.TempDir()
	path := rawtest.Claude(t, root)
	client := newHostedCaptureClient(t, startup, store, parser.AgentClaude, root)
	receipt := client.capture(t)
	waitCaptureJob(t, admin, receipt.ManifestID, "complete", 1)
	search := func(text string) (db.ContentSearchPage, error) {
		return store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: text, Mode: "semantic", IncludeOneShot: true, Limit: 10})
	}
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		page, err := search("Inspect the build")
		if !assert.NoError(c, err) {
			return
		}
		var original bool
		for _, match := range page.Matches {
			original = original || match.SessionID == rawtest.ClaudeID && strings.Contains(match.Snippet, "Inspect the build")
		}
		assert.True(c, original)
	}, 30*time.Second, time.Second)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(content), "Inspect the build.")
	content = bytes.Replace(content, []byte("Inspect the build."), []byte("Review the changed build."), 1)
	require.NoError(t, os.WriteFile(path, content, 0o600))
	next := client.capture(t)
	waitCaptureJob(t, admin, next.ManifestID, "complete", 1)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		page, err := search("Review the changed build")
		if !assert.NoError(c, err) {
			return
		}
		var found bool
		for _, match := range page.Matches {
			found = found || match.SessionID == rawtest.ClaudeID && strings.Contains(match.Snippet, "Review the changed build")
		}
		assert.True(c, found)
	}, 30*time.Second, time.Second)
	configured.PG.RawDerivation = false
	reader, err := prepareReplicaServeImpl(t.Context(), pgReplica{}, configured, "", nil, false)
	require.NoError(t, err)
	defer reader.cleanup()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/search/content?pattern=review&mode=semantic&include_one_shot=true", nil)
	req.Header.Set("Authorization", "Bearer "+configured.AuthToken)
	req.Header.Set("X-AgentsView-Search-Intent", "semantic")
	response := httptest.NewRecorder()
	reader.srv.Handler().ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "Review the changed build")
}

type testHostedRawParser struct{ *rawderive.ProviderParser }

func (p testHostedRawParser) Preflight(ctx context.Context) error { return ctx.Err() }
