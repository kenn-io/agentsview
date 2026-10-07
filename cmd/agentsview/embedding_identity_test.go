package main

import (
	"maps"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/stringutil"
)

func TestEmbeddingIdentityExplicitDefaultsKeepLegacyGoldens(t *testing.T) {
	cfg := config.VectorEmbeddingsConfig{
		Model: "nomic-embed-text", Dimension: 768, MaxInputChars: 8192,
		QueryPrefix: "", DocumentPrefix: "", InputSuffix: "", RequestDimensions: false,
	}
	assert.Equal(t, "97856d475cdedc12", vectorGeneration(cfg).Fingerprint())
	assert.Equal(t, "f5344001f763b46d", recallVectorGeneration(cfg, "extract-v1").Fingerprint())
}

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
		oldSpace, newSpace := vectorSpace(before, oldGen), vectorSpace(after, newGen)
		matches, err := newSpace.Matches(newGen.Fingerprint())
		require.NoError(t, err)
		assert.True(t, matches, "%s must accept its own generation", owner.name)
		matches, err = newSpace.Matches(oldGen.Fingerprint())
		require.NoError(t, err)
		assert.Equal(t, compatible, matches, "%s predecessor compatibility", owner.name)
		oldVector, err := oldSpace.VectorIdentity()
		require.NoError(t, err)
		newVector, err := newSpace.VectorIdentity()
		require.NoError(t, err)
		oldInput, err := oldSpace.InputIdentity()
		require.NoError(t, err)
		newInput, err := newSpace.InputIdentity()
		require.NoError(t, err)
		if compatible {
			assert.Equal(t, oldGen.Fingerprint(), newGen.Fingerprint(), owner.name)
			assert.Equal(t, oldVector, newVector, owner.name)
			assert.Equal(t, oldInput, newInput, owner.name)
		} else {
			assert.NotEqual(t, oldGen.Fingerprint(), newGen.Fingerprint(), owner.name)
			assert.NotEqual(t, oldVector, newVector, owner.name)
			assert.NotEqual(t, oldInput, newInput, owner.name)
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
			"local": {Endpoint: "http://127.0.0.1:8000/v1", BatchSize: 32, Concurrency: 4, Timeout: "30s", MaxRetries: 3},
			"bulk":  {Endpoint: "http://127.0.0.1:8001/v1", BatchSize: 32, Concurrency: 4, Timeout: "30s", MaxRetries: 3},
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

// The invariant is compatibility separation for one changed affix, including
// whitespace and empty transitions. A finite run samples it, not collision freedom.
func FuzzEmbeddingRoleIdentity(f *testing.F) {
	for _, seed := range []struct {
		before, after string
		field         uint8
	}{
		{"", " ", 0},
		{" ", "", 0},
		{"x", "y", 0},
		{"", "title: none | text: ", 1},
		{"title: none | text: ", "", 1},
		{"doc: ", "doc:", 1},
		{"", "<eos>", 2},
		{"<eos>", "", 2},
		{"task: search result | query: ", "task: search result | query:", 0},
		{"\nquery_prefix=x\n", "\nquery_prefix=y\n", 1},
		{"雪", "雪\n", 2},
	} {
		f.Add(seed.before, seed.after, seed.field)
	}
	f.Fuzz(func(t *testing.T, before, after string, field uint8) {
		if !utf8.ValidString(before) || !utf8.ValidString(after) {
			return // role affixes here describe valid UTF-8 text
		}
		// Bound materialized identity work without trimming whitespace or controls.
		before = stringutil.TruncateRunes(before, 256, "")
		after = stringutil.TruncateRunes(after, 256, "")
		if before == after {
			return // bounding can make distinct drawn strings equal
		}
		oldConfig := config.VectorEmbeddingsConfig{
			Model: "embeddinggemma-2-914f7f89-text-768", Dimension: 768, MaxInputChars: 8192,
		}
		newConfig := oldConfig
		switch field % 3 {
		case 0:
			oldConfig.QueryPrefix, newConfig.QueryPrefix = before, after
		case 1:
			oldConfig.DocumentPrefix, newConfig.DocumentPrefix = before, after
		case 2:
			oldConfig.InputSuffix, newConfig.InputSuffix = before, after
		}
		assertEmbeddingIdentityCompatibility(t, oldConfig, newConfig, false)
	})
}
