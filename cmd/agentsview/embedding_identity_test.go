package main

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"

	"go.kenn.io/agentsview/internal/config"
)

// Each owner must reject the old generation when one vector-producing setting
// changes. Operational capacity and transport settings must remain compatible.
func assertEmbeddingIdentityCompatibility(
	t *testing.T, before, after config.VectorEmbeddingsConfig, compatible bool,
) {
	t.Helper()
	for _, owner := range []struct {
		name string
		gen  func(config.VectorEmbeddingsConfig) kitvec.Generation
	}{
		{"messages", vectorGeneration},
		{"recall", func(c config.VectorEmbeddingsConfig) kitvec.Generation {
			return recallVectorGeneration(c, "extract-v1")
		}},
	} {
		oldGen, newGen := owner.gen(before), owner.gen(after)
		newSpace := vectorSpace(after, newGen)
		matches, err := newSpace.Matches(newGen.Fingerprint())
		require.NoError(t, err)
		assert.True(t, matches, "%s must accept its own generation", owner.name)
		matches, err = newSpace.Matches(oldGen.Fingerprint())
		require.NoError(t, err)
		assert.Equal(t, compatible, matches, "%s predecessor compatibility", owner.name)
		if compatible {
			assert.Equal(t, oldGen.Fingerprint(), newGen.Fingerprint(), owner.name)
		} else {
			assert.NotEqual(t, oldGen.Fingerprint(), newGen.Fingerprint(), owner.name)
		}
	}
}

func TestEmbeddingIdentitySeparatesVectorSettings(t *testing.T) {
	baseline := config.VectorEmbeddingsConfig{
		Model: "embeddinggemma-2-914f7f89-text-768", Dimension: 768, MaxInputChars: 8192,
		QueryPrefix: "task: search result | query: ", DocumentPrefix: "title: none | text: ",
	}
	for _, change := range []struct {
		name  string
		apply func(*config.VectorEmbeddingsConfig)
	}{
		{"model alias", func(c *config.VectorEmbeddingsConfig) { c.Model += "-different-recipe" }},
		{"width", func(c *config.VectorEmbeddingsConfig) { c.Dimension = 512 }},
		{"query prefix", func(c *config.VectorEmbeddingsConfig) { c.QueryPrefix = "query: " }},
		{"document prefix", func(c *config.VectorEmbeddingsConfig) { c.DocumentPrefix = "document: " }},
		{"suffix", func(c *config.VectorEmbeddingsConfig) { c.InputSuffix = "<eos>" }},
		{"requested dimensions", func(c *config.VectorEmbeddingsConfig) { c.RequestDimensions = true }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := baseline
			change.apply(&changed)
			assertEmbeddingIdentityCompatibility(t, baseline, changed, false)
		})
	}
}

func TestEmbeddingIdentityIgnoresOperationalSettings(t *testing.T) {
	baseline := config.VectorEmbeddingsConfig{
		Model: "nomic-embed-text", Dimension: 768, MaxInputChars: 8192,
		Servers: map[string]config.VectorEmbeddingsServerConfig{
			"local": {
				Endpoint: "http://127.0.0.1:8000/v1", BatchSize: 32, Concurrency: 4,
				Timeout: "30s", MaxRetries: 3,
			},
			"bulk": {
				Endpoint: "http://127.0.0.1:8001/v1", BatchSize: 32, Concurrency: 4,
				Timeout: "30s", MaxRetries: 3,
			},
		},
		DefaultServer: "local",
	}
	for _, change := range []struct {
		name  string
		apply func(*config.VectorEmbeddingsConfig)
	}{
		{"endpoint", func(c *config.VectorEmbeddingsConfig) {
			c.Servers = maps.Clone(c.Servers)
			server := c.Servers["local"]
			server.Endpoint = "http://127.0.0.1:9000/v1"
			c.Servers["local"] = server
		}},
		{"default server", func(c *config.VectorEmbeddingsConfig) { c.DefaultServer = "bulk" }},
		{"transport and capacity", func(c *config.VectorEmbeddingsConfig) {
			c.Servers = maps.Clone(c.Servers)
			server := c.Servers["local"]
			server.BatchSize, server.Concurrency, server.Timeout, server.MaxRetries = 4, 1, "120s", 0
			c.Servers["local"] = server
		}},
		{"model context capacity", func(c *config.VectorEmbeddingsConfig) { c.ModelContextTokens = 8192 }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := baseline
			change.apply(&changed)
			assertEmbeddingIdentityCompatibility(t, baseline, changed, true)
		})
	}
}
