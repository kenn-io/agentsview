package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validVectorConfig returns a VectorConfig with every field enabled
// and populated with values that pass Validate, so each table case
// below can mutate exactly the field under test.
func validVectorConfig() VectorConfig {
	return VectorConfig{
		Enabled: true,
		Embeddings: VectorEmbeddingsConfig{
			Endpoint:  "http://localhost:11434/v1",
			Model:     "nomic-embed-text",
			Dimension: 768,
			Timeout:   "30s",
		},
		Embed: VectorEmbedConfig{
			BackstopInterval: "24h",
		},
	}
}

func TestVectorConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*VectorConfig)
		wantErr string
	}{
		{
			name:   "disabled is valid even with empty fields",
			mutate: func(c *VectorConfig) { *c = VectorConfig{} },
		},
		{
			name:    "enabled missing endpoint",
			mutate:  func(c *VectorConfig) { c.Embeddings.Endpoint = "" },
			wantErr: "endpoint is required",
		},
		{
			name:    "enabled missing model",
			mutate:  func(c *VectorConfig) { c.Embeddings.Model = "" },
			wantErr: "model is required",
		},
		{
			name:    "enabled missing dimension",
			mutate:  func(c *VectorConfig) { c.Embeddings.Dimension = 0 },
			wantErr: "dimension",
		},
		{
			name:    "enabled negative dimension",
			mutate:  func(c *VectorConfig) { c.Embeddings.Dimension = -1 },
			wantErr: "dimension",
		},
		{
			name:    "enabled bad timeout",
			mutate:  func(c *VectorConfig) { c.Embeddings.Timeout = "not-a-duration" },
			wantErr: "timeout",
		},
		{
			name:    "enabled bad backstop interval",
			mutate:  func(c *VectorConfig) { c.Embed.BackstopInterval = "not-a-duration" },
			wantErr: "backstop_interval",
		},
		{
			name:   "enabled negative backstop interval disables and is valid",
			mutate: func(c *VectorConfig) { c.Embed.BackstopInterval = "-1s" },
		},
		{
			name: "fully valid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validVectorConfig()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			err := cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestVectorConfigDefaults(t *testing.T) {
	cfg, err := Default()
	require.NoError(t, err)

	assert.Equal(t, 32, cfg.Vector.Embeddings.BatchSize)
	assert.Equal(t, "30s", cfg.Vector.Embeddings.Timeout)
	assert.Equal(t, 3, cfg.Vector.Embeddings.MaxRetries)
	assert.Equal(t, 8192, cfg.Vector.Embeddings.MaxInputChars)
	assert.True(t, cfg.Vector.Embed.RunAfterSyncEnabled(),
		"run_after_sync defaults to true when unset")

	disabled := false
	cfg.Vector.Embed.RunAfterSync = &disabled
	assert.False(t, cfg.Vector.Embed.RunAfterSyncEnabled(),
		"explicit false overrides the default")

	assert.Equal(t, filepath.Join(cfg.DataDir, "vectors.db"),
		cfg.Vector.ResolvedDBPath(cfg.DataDir), "falls back to <dataDir>/vectors.db")

	cfg.Vector.DBPath = "/custom/path/vec.db"
	assert.Equal(t, "/custom/path/vec.db", cfg.Vector.ResolvedDBPath(cfg.DataDir),
		"explicit db_path overrides the fallback")
}

func TestVectorConfigAPIKeyEnv(t *testing.T) {
	embeddings := VectorEmbeddingsConfig{}
	assert.Equal(t, "", embeddings.APIKey(), "no env var configured")

	embeddings.APIKeyEnv = "AGENTSVIEW_TEST_VECTOR_API_KEY"
	assert.Equal(t, "", embeddings.APIKey(), "configured env var not set in environment")

	t.Setenv("AGENTSVIEW_TEST_VECTOR_API_KEY", "secret-123")
	assert.Equal(t, "secret-123", embeddings.APIKey())
}

// TestVectorConfigTOMLLoad exercises the full config-file load path so the
// default-merge logic in applyConfigTOML (not just the section types in
// isolation) is covered, including the ability to explicitly override a
// zero-value field like max_retries.
func TestVectorConfigTOMLLoad(t *testing.T) {
	t.Run("unset fields keep defaults, explicit zero overrides", func(t *testing.T) {
		cfg := loadMinimalWithConfig(t, map[string]any{
			"vector": map[string]any{
				"enabled": true,
				"embeddings": map[string]any{
					"endpoint":    "http://localhost:11434/v1",
					"model":       "nomic-embed-text",
					"dimension":   768,
					"max_retries": 0,
				},
			},
		})
		require.True(t, cfg.Vector.Enabled)
		assert.Equal(t, "http://localhost:11434/v1", cfg.Vector.Embeddings.Endpoint)
		assert.Equal(t, 32, cfg.Vector.Embeddings.BatchSize, "unset batch_size keeps default")
		assert.Equal(t, "30s", cfg.Vector.Embeddings.Timeout, "unset timeout keeps default")
		assert.Equal(t, 0, cfg.Vector.Embeddings.MaxRetries, "explicit max_retries=0 overrides default")
		assert.Equal(t, 8192, cfg.Vector.Embeddings.MaxInputChars, "unset max_input_chars keeps default")
		assert.Equal(t, "24h", cfg.Vector.Embed.BackstopInterval, "unset backstop_interval keeps default")
	})

	t.Run("enabled without required fields fails to load", func(t *testing.T) {
		err := loadMinimalErrWithConfig(t, map[string]any{
			"vector": map[string]any{"enabled": true},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "endpoint is required")
	})

	t.Run("disabled section with no fields loads fine", func(t *testing.T) {
		cfg := loadMinimalWithConfig(t, map[string]any{
			"vector": map[string]any{},
		})
		assert.False(t, cfg.Vector.Enabled)
	})
}
