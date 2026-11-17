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

func TestHostedCaptureSemanticSearch(t *testing.T) {
	t.Run("in-process parser", func(t *testing.T) { hostedCaptureSemanticSearch(t, true) })
	t.Run("sandboxed runtime", func(t *testing.T) { hostedCaptureSemanticSearch(t, false) })
}

type testHostedRawParser struct{ *rawderive.ProviderParser }

func (p testHostedRawParser) Preflight(ctx context.Context) error { return ctx.Err() }

func hostedCaptureSemanticSearch(t *testing.T, manual bool) {
	if manual {
		original := newHostedRawParser
		newHostedRawParser = func(time.Duration) (hostedRawParser, error) {
			p, err := rawderive.NewProviderParser(parser.ProviderFactories(), "hosted")
			return testHostedRawParser{p}, err
		}
		t.Cleanup(func() { newHostedRawParser = original })
	}
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
	startup, store, admin := startParityRuntimeConfigured(t, config.ArchiveContentFull, !manual, func(cfg *config.Config, admin *sql.DB) {
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
	search := func(text string) db.ContentSearchPage {
		page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: text, Mode: "semantic", IncludeOneShot: true, Limit: 10})
		if err != nil {
			return db.ContentSearchPage{}
		}
		return page
	}
	require.Eventually(t, func() bool { return len(search("Inspect the build").Matches) > 0 }, 30*time.Second, time.Second)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(content), "Inspect the build.")
	content = bytes.Replace(content, []byte("Inspect the build."), []byte("Review the changed build."), 1)
	require.NoError(t, os.WriteFile(path, content, 0600))
	next := client.capture(t)
	waitCaptureJob(t, admin, next.ManifestID, "complete", 1)
	require.Eventually(t, func() bool {
		var count int
		err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_documents WHERE content LIKE '%Review the changed build%'`).Scan(&count)
		return err == nil && count == 1
	}, 30*time.Second, time.Second)
	page := search("Review the changed build")
	require.NotEmpty(t, page.Matches)
	assert.Equal(t, rawtest.ClaudeID, page.Matches[0].SessionID)
	var old int
	require.NoError(t, admin.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_documents WHERE content LIKE '%Inspect the build%'`).Scan(&old))
	assert.Zero(t, old)
}
