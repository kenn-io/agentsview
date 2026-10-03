package main

import (
	"context"
	"fmt"
	"maps"
	"strconv"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/storage"
)

// adoptReplicaVectorConfig returns cfg with the single embedding recipe
// published to a PostgreSQL replica, served through the deployment
// embeddings server. A local [vector] section always wins, and cfg is
// returned unchanged without a deployment server or on other backends.
// Nothing is written to config.toml; callers keep the adopted value for
// their lifetime.
func adoptReplicaVectorConfig(
	ctx context.Context, cfg config.Config,
	backend storage.Replica, store storage.ReplicaStore,
) (config.Config, error) {
	if cfg.Vector.Enabled || cfg.DeploymentEmbeddings == nil || backend.Name() != "pg" {
		return cfg, nil
	}
	provider, ok := backend.(storage.VectorSearchProvider)
	if !ok {
		return cfg, fmt.Errorf("%s does not publish embedding recipes", backend.DisplayName())
	}
	gens, err := provider.VectorGenerations(ctx, store)
	if err != nil {
		return cfg, fmt.Errorf("reading published embedding recipes: %w", err)
	}
	var recipes []config.VectorEmbeddingsConfig
	for _, gen := range gens {
		if recipe, ok := decodeReplicaRecipe(gen); ok {
			recipes = append(recipes, recipe)
		}
	}
	if len(recipes) != 1 {
		return cfg, fmt.Errorf(
			"semantic recipe adoption requires exactly one supported published "+
				"generation (found %d); push from a configured workstation first, "+
				"remove obsolete generations with 'agentsview pg vectors drop <id>', "+
				"or configure [vector.embeddings] and restart", len(recipes))
	}
	recipe := recipes[0]
	recipe.DefaultServer = "deployment"
	recipe.Servers = map[string]config.VectorEmbeddingsServerConfig{
		"deployment": *cfg.DeploymentEmbeddings,
	}
	cfg.Vector.Embeddings = recipe
	cfg.Vector.Enabled = true
	if err := cfg.Vector.Validate(); err != nil {
		return cfg, fmt.Errorf("adopted embedding recipe: %w", err)
	}
	return cfg, nil
}

// decodeReplicaRecipe rebuilds the embeddings config a published generation
// was built with. It accepts a generation only when the decoded recipe
// reproduces the generation's fingerprint and stored params exactly, so
// unknown or altered params are never trusted.
func decodeReplicaRecipe(gen storage.VectorGenerationInfo) (config.VectorEmbeddingsConfig, bool) {
	if gen.Model == "" || gen.Dimension <= 0 || gen.Params == nil {
		return config.VectorEmbeddingsConfig{}, false
	}
	maxChars, err := strconv.Atoi(gen.Params["max_input_chars"])
	if err != nil || maxChars <= 0 {
		return config.VectorEmbeddingsConfig{}, false
	}
	c := config.VectorEmbeddingsConfig{
		Model:             gen.Model,
		Dimension:         gen.Dimension,
		MaxInputChars:     maxChars,
		QueryPrefix:       gen.Params["query_prefix"],
		DocumentPrefix:    gen.Params["document_prefix"],
		InputSuffix:       gen.Params["input_suffix"],
		RequestDimensions: gen.Params["request_dimensions"] == "true",
	}
	rebuilt := vectorGeneration(c)
	if rebuilt.Fingerprint() != gen.Fingerprint || !maps.Equal(rebuilt.Params, gen.Params) {
		return config.VectorEmbeddingsConfig{}, false
	}
	return c, true
}
