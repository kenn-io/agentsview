//go:build pgtest

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		{name: "missing generation", vector: defaults.Vector, enabled: true, want: "no embedding generation matching fingerprint"},
		{name: "missing deployment recipe", server: &config.VectorEmbeddingsServerConfig{Endpoint: "http://example.test"}, want: "semantic recipe adoption requires exactly one supported published generation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.Vector = tc.vector
			cfg.Vector.Enabled = tc.enabled
			cfg.DeploymentEmbeddings = tc.server
			require.NoError(t, wireReplicaVectorSearch(t.Context(), cfg, pgReplica{}, store, "hosted"))
			_, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "hello", Mode: "semantic"})
			require.ErrorIs(t, err, db.ErrSemanticUnavailable)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "hosted-provision")
			assert.NotContains(t, err.Error(), "replica pg push")
			assert.NotContains(t, err.Error(), "configured workstation")
		})
	}
}

func TestHostedReadOnlyStartsWithoutVectorGeneration(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	_, err := admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	database, err := postgres.OpenHosted(t.Context(), cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision,message_count,user_message_count) VALUES('session','p','m','codex','raw','rev',1,1); INSERT INTO messages(session_id,ordinal,role,content) VALUES('session',0,'user','visible text')`)
	require.NoError(t, err)
	defaults, err := config.Default()
	require.NoError(t, err)
	cfg.Vector = defaults.Vector
	cfg.Vector.Enabled = true
	cfg.Vector.Embeddings.Model = "fixture"
	cfg.Vector.Embeddings.Dimension = 2
	cfg.Vector.Embeddings.DefaultServer = "fixture"
	cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"fixture": {Endpoint: "http://example.test", Timeout: "30s"}}
	startup, err := prepareReplicaServeImpl(t.Context(), pgReplica{}, cfg, "", nil, false)
	require.NoError(t, err)
	t.Cleanup(startup.cleanup)
	require.Nil(t, startup.startWorker)
	request := func(mode string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/search/content?pattern=visible&mode="+mode+"&include_one_shot=true", nil)
		req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
		req.Header.Set("X-AgentsView-Search-Intent", "semantic")
		response := httptest.NewRecorder()
		startup.srv.Handler().ServeHTTP(response, req)
		return response
	}
	lexical := request("substring")
	assert.Equal(t, http.StatusOK, lexical.Code, lexical.Body.String())
	assert.Contains(t, lexical.Body.String(), "visible text")
	semantic := request("semantic")
	assert.Equal(t, http.StatusNotImplemented, semantic.Code, semantic.Body.String())
	startup.cleanup()
	cfg.PG.RawDerivation = true
	workerStartup, err := prepareReplicaServeImpl(t.Context(), pgReplica{}, cfg, "", nil, false)
	if err == nil {
		workerStartup.cleanup()
	}
	require.ErrorContains(t, err, "hosted embedding generation is missing")
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

func TestHostedEmbeddingsRateLimitDoesNotBlockNextSession(t *testing.T) {
	cfg, admin := hostedRuntimeConfig(t)
	defaults, err := config.Default()
	require.NoError(t, err)
	cfg.Vector = defaults.Vector
	cfg.Vector.Enabled = true
	var rateCalls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.Contains(req.Input[0], "rate limited") && rateCalls.Add(1) == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0}}}, "model": "fixture", "object": "list"})
	}))
	t.Cleanup(endpoint.Close)
	cfg.Vector.Embeddings.Model = "fixture"
	cfg.Vector.Embeddings.Dimension = 2
	cfg.Vector.Embeddings.MaxInputChars = 128
	cfg.Vector.Embeddings.DefaultServer = "fixture"
	cfg.Vector.Embeddings.Servers = map[string]config.VectorEmbeddingsServerConfig{"fixture": {Endpoint: endpoint.URL, BatchSize: 1, Concurrency: 1, MaxRetries: 1, Timeout: "30s"}}
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
	store, err := postgres.NewHostedStore(t.Context(), cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.DB().ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('a','p','m','codex','raw','rev'),('b','p','m','codex','raw','rev'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('a',0,'user','rate limited'),('b',0,'user','good content')`)
	require.NoError(t, err)
	runtime, err := prepareHostedEmbeddings(t.Context(), cfg, store, time.Second)
	require.NoError(t, err)
	defer runtime.Stop()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	runtime.batch(ctx)
	var count int
	runtime.batch(ctx)
	require.EqualValues(t, 2, rateCalls.Load(), "the failed session retries on the next poll")
	require.NoError(t, store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM vector_push_state WHERE session_id='a'`).Scan(&count))
	assert.Equal(t, 1, count)
	runtime.batch(ctx)
	assert.EqualValues(t, 2, rateCalls.Load(), "a completed revision is not embedded again")
	_, err = store.DB().ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent,provenance_kind,raw_content_revision) VALUES('c','p','m','codex','raw','rev'); INSERT INTO messages(session_id,ordinal,role,content) VALUES('c',0,'user','new content'); INSERT INTO raw_corpus_state(singleton,corpus_revision) VALUES(1,1) ON CONFLICT(singleton) DO UPDATE SET corpus_revision=raw_corpus_state.corpus_revision+1`)
	require.NoError(t, err)
	runtime.batch(ctx)
	require.NoError(t, store.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM vector_push_state WHERE session_id='c'`).Scan(&count))
	assert.Equal(t, 1, count, "a new corpus revision starts another pass")
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
	var authToken string
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
		authToken = cfg.AuthToken
	})
	root := t.TempDir()
	rawtest.Claude(t, root)
	client := newHostedCaptureClient(t, startup, store, parser.AgentClaude, root)
	receipt := client.capture(t)
	waitCaptureJob(t, admin, receipt.ManifestID, "complete", 1)
	search := func(text string) (db.ContentSearchPage, error) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/search/content?pattern="+url.QueryEscape(text)+"&mode=semantic&include_one_shot=true", nil)
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("X-AgentsView-Search-Intent", "semantic")
		response := httptest.NewRecorder()
		startup.srv.Handler().ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			return db.ContentSearchPage{}, fmt.Errorf("search returned %d: %s", response.Code, response.Body.String())
		}
		var page db.ContentSearchPage
		err := json.NewDecoder(response.Body).Decode(&page)
		return page, err
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

}

type testHostedRawParser struct{ *rawderive.ProviderParser }

func (p testHostedRawParser) Preflight(ctx context.Context) error { return ctx.Err() }
