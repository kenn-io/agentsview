package main

import (
	"context"
	"errors"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// recipeProvider is a PostgreSQL-named replica that lists fixed generations.
type recipeProvider struct {
	storage.Replica
	gens    []storage.VectorGenerationInfo
	listErr error
}

func (recipeProvider) Name() string        { return "pg" }
func (recipeProvider) DisplayName() string { return "PostgreSQL" }

func (p recipeProvider) VectorGenerations(
	context.Context, storage.ReplicaStore,
) ([]storage.VectorGenerationInfo, error) {
	return p.gens, p.listErr
}

func (recipeProvider) OpenVectorSearcher(
	context.Context, storage.ReplicaStore, storage.VectorGenerationInfo, int,
	storage.VectorQueryEncoder,
) (db.VectorSearcher, string, error) {
	panic("not used by recipe adoption")
}

func publishedRecipeConfig(model string) config.VectorEmbeddingsConfig {
	return config.VectorEmbeddingsConfig{
		Model: model, Dimension: 4, MaxInputChars: 8192,
		QueryPrefix: "query: ", DocumentPrefix: "passage: ", RequestDimensions: true,
	}
}

// publishedRecipe is a generation row as a configured workstation's push
// publishes it.
func publishedRecipe() storage.VectorGenerationInfo {
	gen := vectorGeneration(publishedRecipeConfig("published-model"))
	return storage.VectorGenerationInfo{
		Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: gen.Dimensions,
		Params: maps.Clone(gen.Params),
	}
}

func deploymentRecipeConfig() config.Config {
	return config.Config{
		Vector: config.VectorConfig{Embed: config.VectorEmbedConfig{BackstopInterval: "24h"}},
		DeploymentEmbeddings: &config.VectorEmbeddingsServerConfig{
			Endpoint: "http://embeddings.test/v1", BatchSize: 32, Concurrency: 4,
			Timeout: "30s", MaxRetries: 3,
		},
	}
}

func TestAdoptReplicaRecipe(t *testing.T) {
	cfg := deploymentRecipeConfig()
	gen := publishedRecipe()
	legacy := storage.VectorGenerationInfo{Fingerprint: "legacy", Model: "old", Dimension: 4}
	got, err := adoptReplicaVectorConfig(t.Context(), cfg,
		recipeProvider{gens: []storage.VectorGenerationInfo{legacy, gen}}, nil)
	require.NoError(t, err)
	assert.True(t, got.Vector.Enabled)
	assert.Equal(t, publishedRecipeConfig("published-model").QueryPrefix, got.Vector.Embeddings.QueryPrefix)
	assert.Equal(t, "passage: ", got.Vector.Embeddings.DocumentPrefix)
	assert.Equal(t, 8192, got.Vector.Embeddings.MaxInputChars)
	assert.True(t, got.Vector.Embeddings.RequestDimensions)
	assert.Equal(t, gen.Fingerprint, vectorGeneration(got.Vector.Embeddings).Fingerprint())
	assert.Equal(t, "deployment", got.Vector.Embeddings.ResolvedDefaultServer())
	assert.Equal(t, *cfg.DeploymentEmbeddings, got.Vector.Embeddings.Servers["deployment"])
	assert.False(t, cfg.Vector.Enabled, "adoption must not mutate the caller")
}

func TestAdoptReplicaRecipeFailsClosed(t *testing.T) {
	other := func() storage.VectorGenerationInfo {
		gen := vectorGeneration(publishedRecipeConfig("other-model"))
		return storage.VectorGenerationInfo{
			Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: 4, Params: gen.Params,
		}
	}
	for _, tc := range []struct {
		name    string
		mutate  func([]storage.VectorGenerationInfo) []storage.VectorGenerationInfo
		listErr error
		want    string
	}{
		{name: "zero", mutate: func([]storage.VectorGenerationInfo) []storage.VectorGenerationInfo { return nil }, want: "found 0"},
		{name: "two", mutate: func(g []storage.VectorGenerationInfo) []storage.VectorGenerationInfo { return append(g, other()) }, want: "found 2"},
		{name: "legacy null params", mutate: func(g []storage.VectorGenerationInfo) []storage.VectorGenerationInfo {
			g[0].Params = nil
			return g
		}, want: "found 0"},
		{name: "fingerprint mismatch", mutate: func(g []storage.VectorGenerationInfo) []storage.VectorGenerationInfo {
			g[0].Params["query_prefix"] = "search: "
			return g
		}, want: "found 0"},
		{name: "unknown param", mutate: func(g []storage.VectorGenerationInfo) []storage.VectorGenerationInfo {
			g[0].Params["future_recipe"] = "x"
			return g
		}, want: "found 0"},
		{name: "overlap", mutate: func(g []storage.VectorGenerationInfo) []storage.VectorGenerationInfo {
			g[0].Params["chunk_overlap_chars"] = "0"
			return g
		}, want: "found 0"},
		{name: "zero dimension", mutate: func(g []storage.VectorGenerationInfo) []storage.VectorGenerationInfo {
			g[0].Dimension = 0
			return g
		}, want: "found 0"},
		{name: "list error", listErr: errors.New("connection refused"), want: "connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gens := []storage.VectorGenerationInfo{publishedRecipe()}
			if tc.mutate != nil {
				gens = tc.mutate(gens)
			}
			got, err := adoptReplicaVectorConfig(t.Context(), deploymentRecipeConfig(),
				recipeProvider{gens: gens, listErr: tc.listErr}, nil)
			require.ErrorContains(t, err, tc.want)
			assert.False(t, got.Vector.Enabled)
		})
	}
}

func TestExplicitVectorConfigWins(t *testing.T) {
	explicit := vectorTestConfig(t.TempDir())
	explicit.DeploymentEmbeddings = deploymentRecipeConfig().DeploymentEmbeddings
	provider := recipeProvider{gens: []storage.VectorGenerationInfo{publishedRecipe()}}
	got, err := adoptReplicaVectorConfig(t.Context(), explicit, provider, nil)
	require.NoError(t, err)
	assert.Equal(t, explicit, got, "local [vector] wins over a published recipe")

	noServer := deploymentRecipeConfig()
	noServer.DeploymentEmbeddings = nil
	got, err = adoptReplicaVectorConfig(t.Context(), noServer, provider, nil)
	require.NoError(t, err)
	assert.False(t, got.Vector.Enabled, "adoption needs a deployment embeddings server")
}

func TestDecodeReplicaRecipeRoundTrip(t *testing.T) {
	for _, c := range []config.VectorEmbeddingsConfig{
		{Model: "m", Dimension: 8, MaxInputChars: 1000},
		{Model: "m", Dimension: 8, MaxInputChars: 2000, InputSuffix: "</s>"},
		publishedRecipeConfig("m"),
	} {
		gen := vectorGeneration(c)
		got, ok := decodeReplicaRecipe(storage.VectorGenerationInfo{
			Fingerprint: gen.Fingerprint(), Model: gen.Model, Dimension: gen.Dimensions,
			Params: gen.Params,
		})
		require.True(t, ok)
		assert.Equal(t, c, got)
	}
}
