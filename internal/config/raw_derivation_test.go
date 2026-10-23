package config

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestRawDerivationConfigBindingAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		p     PGConfig
		auth  bool
		valid bool
	}{
		{"off", PGConfig{}, false, true},
		{"missing tenant", PGConfig{RawDerivation: true}, true, false},
		{"missing auth", PGConfig{RawTenant: "tenant", RawDerivation: true, Schema: "hosted"}, false, false},
		{"valid", PGConfig{RawTenant: "tenant", RawDerivation: true, Schema: "hosted"}, true, true},
		{"negative poll", PGConfig{RawTenant: "tenant", RawDerivation: true, Schema: "hosted", RawPollSeconds: -1}, true, false},
		{"unbounded attempt", PGConfig{RawTenant: "tenant", RawDerivation: true, Schema: "hosted", RawAttemptSeconds: 301}, true, false},
		{"unbounded retries", PGConfig{RawTenant: "tenant", RawDerivation: true, Schema: "hosted", RawMaxAttempts: 11}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.ValidateRawDerivation(tc.auth)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	legacy, targets, err := parsePGConfigSection(map[string]any{"raw_tenant": "tenant", "raw_derivation": true, "raw_max_attempts": int64(3)})
	require.NoError(t, err)
	assert.Empty(t, targets)
	assert.Equal(t, "tenant", legacy.RawTenant)
	assert.True(t, legacy.RawDerivation)
	assert.Equal(t, 3, legacy.RawMaxAttempts)
	_, targets, err = parsePGConfigSection(map[string]any{"remote": map[string]any{"raw_tenant": "tenant", "raw_derivation": true}})
	require.NoError(t, err)
	assert.True(t, targets["remote"].RawDerivation)
	_, _, err = parsePGConfigSection(map[string]any{"raw_tenant": map[string]any{"url": "invalid"}})
	assert.Error(t, err)
}

func TestRawEmbeddingsRequiresDerivation(t *testing.T) {
	require.ErrorContains(t, (PGConfig{RawEmbeddings: true}).ValidateRawDerivation(true), "raw_derivation")
	_, targets, err := parsePGConfigSection(map[string]any{"hosted": map[string]any{"raw_embeddings": true, "raw_derivation": true}})
	require.NoError(t, err)
	assert.True(t, targets["hosted"].RawEmbeddings)
}

func TestHostedEmbeddingsValidation(t *testing.T) {
	transport := VectorEmbeddingsServerConfig{Endpoint: "https://embeddings.example.com/v1", BatchSize: 32, Concurrency: 4, Timeout: "30s"}
	recipe := VectorEmbeddingsConfig{Model: "synthetic-model", Dimension: 4, MaxInputChars: 64}
	for _, tc := range []struct {
		name      string
		config    Config
		provision bool
		want      string
	}{
		{name: "missing owner recipe", provision: true, want: "[vector.embeddings]"},
		{name: "transport cannot provision recipe", config: Config{DeploymentEmbeddings: &transport}, provision: true, want: "[vector.embeddings]"},
		{name: "owner recipe needs no transport", config: Config{Vector: VectorConfig{Embeddings: recipe}}, provision: true},
		{name: "missing runtime transport", want: "server"},
		{name: "deployment runtime", config: Config{DeploymentEmbeddings: &transport}},
		{name: "configured runtime transport", config: Config{Vector: VectorConfig{Embeddings: VectorEmbeddingsConfig{Servers: map[string]VectorEmbeddingsServerConfig{"primary": transport}}}}},
		{name: "bad runtime transport", config: Config{DeploymentEmbeddings: &VectorEmbeddingsServerConfig{}}, want: "endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.config.ValidatePGRawEmbeddings(PGConfig{RawEmbeddings: true}, tc.provision)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
			require.NoError(t, tc.config.ValidatePGRawEmbeddings(PGConfig{}, tc.provision))
		})
	}
}

func TestLoadRawEmbeddingsTargets(t *testing.T) {
	for _, section := range []string{"pg", "pg.hosted"} {
		t.Run(section, func(t *testing.T) {
			dir := setupTestEnv(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("["+section+"]\nurl = 'postgres://db.example.com/archive'\nschema = 'hosted'\nraw_tenant = 'tenant'\nraw_derivation = true\nraw_embeddings = true\n"), 0o600))
			cfg, err := LoadMinimalFor(ScopePG)
			require.NoError(t, err)
			target := ""
			if section == "pg.hosted" {
				target = "hosted"
			}
			pg, err := cfg.ResolvePGTarget(target)
			require.NoError(t, err)
			assert.Equal(t, "tenant", pg.RawTenant)
			assert.True(t, pg.RawDerivation)
			assert.True(t, pg.RawEmbeddings)
		})
	}
}

func TestHostedRuntimeTransportIgnoresLocalRecipe(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		server := VectorEmbeddingsServerConfig{Endpoint: "https://embeddings.example.com/v1", BatchSize: 32, MaxBatchTokens: hegel.Draw(ht, hegel.Integers(1, math.MaxInt)), Concurrency: 4, Timeout: "30s"}
		recipe := VectorEmbeddingsConfig{Model: hegel.Draw(ht, hegel.Text()), Dimension: hegel.Draw(ht, hegel.Integers(math.MinInt, math.MaxInt)), ModelContextTokens: hegel.Draw(ht, hegel.Integers(math.MinInt, math.MaxInt)), Servers: map[string]VectorEmbeddingsServerConfig{"primary": server}}
		cfg := Config{Vector: VectorConfig{Embeddings: recipe}}
		require.NoError(ht, cfg.ValidatePGRawEmbeddings(PGConfig{RawEmbeddings: true}, false))
		cfg.Vector.Embeddings.Servers = nil
		cfg.DeploymentEmbeddings = &server
		require.NoError(ht, cfg.ValidatePGRawEmbeddings(PGConfig{RawEmbeddings: true}, false))
	}, hegel.WithTestCases(2000))
}

func TestHostedProvisionRejectsNegativeContext(t *testing.T) {
	cfg := Config{Vector: VectorConfig{Embeddings: VectorEmbeddingsConfig{Model: "synthetic", Dimension: 4, MaxInputChars: 64, ModelContextTokens: -1}}}
	require.ErrorContains(t, cfg.ValidatePGRawEmbeddings(PGConfig{RawEmbeddings: true}, true), "model_context_tokens")
}

func TestHostedProvisionDimensionBounds(t *testing.T) {
	cfg := Config{Vector: VectorConfig{Embeddings: VectorEmbeddingsConfig{Model: "synthetic", MaxInputChars: 64}}}
	for _, dimension := range []int{4000, 4001, math.MaxInt} {
		cfg.Vector.Embeddings.Dimension = dimension
		err := cfg.ValidatePGRawEmbeddings(PGConfig{RawEmbeddings: true}, true)
		if dimension == 4000 {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, "4000")
		}
	}
}
