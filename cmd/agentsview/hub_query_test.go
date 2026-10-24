package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/vector"
)

func TestHubLocalQueryUsesSavedRecipeWithoutHubCredentials(t *testing.T) {
	cfg, database, hubCalls := hubProducerFixture(t)
	ix, err := vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), false, 1000)
	require.NoError(t, err)
	_, err = hubProducerBuild(ix, database, cfg)(t.Context(), vector.BuildRequest{}, nil)
	require.NoError(t, err)
	_, err = ix.Pull(t.Context(), database.LocalEmbeddableSource(), unavailableHubCoverage{}, vectorGeneration(publishedRecipeConfig("next-model")), vector.PullOptions{})
	require.NoError(t, err)
	require.NoError(t, ix.Close())
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model      string   `json:"model"`
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		require.NoError(t, json.UnmarshalRead(r.Body, &body))
		require.Equal(t, "hub-model", body.Model)
		require.Equal(t, []string{"query: needle"}, body.Input)
		require.Equal(t, 4, body.Dimensions)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.MarshalWrite(w, map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0, 0, 0}}}}))
	}))
	t.Cleanup(endpoint.Close)
	cfg.Remote.URL = "http://127.0.0.1:1"
	cfg.Remote.TokenFile = filepath.Join(t.TempDir(), "missing-hub-token")
	cfg.Vector.Embeddings = config.VectorEmbeddingsConfig{
		Model: "conflicting-model", Dimension: 123, QueryPrefix: "wrong: ", MaxInputChars: 1,
		DocumentPrefix: "wrong passage: ", InputSuffix: "wrong suffix", RequestDimensions: false,
		RecipeKeys: map[string]bool{"model": true, "dimension": true, "query_prefix": true, "document_prefix": true, "input_suffix": true, "max_input_chars": true, "request_dimensions": true},
		Servers:    map[string]config.VectorEmbeddingsServerConfig{"query": {Endpoint: endpoint.URL + "/v1", Timeout: "5s", MaxRetries: 1}},
	}
	closeSearch := installDirectVectorSearcher(cfg, database)
	require.NotNil(t, closeSearch)
	t.Cleanup(func() { require.NoError(t, closeSearch()) })
	ready, err := database.SemanticReadiness(t.Context())
	require.NoError(t, err)
	require.Equal(t, "ready", ready.State)
	require.EqualValues(t, 1, ready.Embedded)
	page, err := database.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "needle", Mode: "semantic", IncludeOneShot: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	require.Equal(t, "local", page.Matches[0].SessionID)
	require.EqualValues(t, 1, hubCalls.Load(), "local query must not contact the hub")
}

type unavailableHubCoverage struct{}

func (unavailableHubCoverage) LookupHubVectors(_ context.Context, r vector.HubLookupRequest) (vector.HubLookupReply, error) {
	absent := make([]int, len(r.Identities))
	for i := range absent {
		absent[i] = i
	}
	return vector.HubLookupReply{Version: vector.HubVectorVersion, Fingerprint: r.Fingerprint, Absent: absent}, nil
}

func TestHubLocalQueryUnavailable(t *testing.T) {
	for _, missing := range []string{"index", "encoder"} {
		t.Run(missing, func(t *testing.T) {
			cfg, database, calls := hubProducerFixture(t)
			if missing == "encoder" {
				ix, err := vector.Open(t.Context(), cfg.Vector.ResolvedDBPath(cfg.DataDir), false, 1000)
				require.NoError(t, err)
				_, err = hubProducerBuild(ix, database, cfg)(t.Context(), vector.BuildRequest{}, nil)
				require.NoError(t, err)
				require.NoError(t, ix.Close())
				cfg.Vector.Embeddings.Servers = nil
			}
			before := calls.Load()
			cfg.Remote.TokenFile = filepath.Join(t.TempDir(), "missing-token")
			if closeSearch := installDirectVectorSearcher(cfg, database); closeSearch != nil {
				defer closeSearch()
			}
			_, err := database.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "needle", Mode: "semantic", IncludeOneShot: true, Limit: 10})
			require.ErrorIs(t, err, db.ErrSemanticUnavailable)
			require.ErrorContains(t, err, "semantic search unavailable locally")
			page, err := database.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "content", IncludeOneShot: true, Limit: 10})
			require.NoError(t, err)
			require.NotEmpty(t, page.Matches)
			require.Equal(t, before, calls.Load())
		})
	}
}

func TestHubDaemonSetupAllowsOmittedRecipe(t *testing.T) {
	cfg, database, _ := hubProducerFixture(t)
	cfg.Vector.Embeddings.Model = ""
	cfg.Vector.Embeddings.Dimension = 0
	cfg.Vector.Embed.Recall = false
	serving, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(t, err)
	require.NotNil(t, serving.Close)
	t.Cleanup(func() { require.NoError(t, serving.Close()) })
	_, err = database.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: "needle", Mode: "semantic", IncludeOneShot: true, Limit: 10})
	require.ErrorIs(t, err, db.ErrSemanticUnavailable)
	require.ErrorContains(t, err, "semantic search unavailable locally")
}
