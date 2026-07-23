package config

import (
	"maps"
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
			Model:         "nomic-embed-text",
			Dimension:     768,
			MaxInputChars: 8192,
			DefaultServer: "local",
			Servers: map[string]VectorEmbeddingsServerConfig{
				"local": {
					Endpoint:    "http://localhost:11434/v1",
					Timeout:     "30s",
					BatchSize:   32,
					Concurrency: 4,
					MaxRetries:  3,
				},
				"remote": {
					Endpoint:    "http://build-box:30000/v1",
					Timeout:     "300s",
					BatchSize:   32,
					Concurrency: 6,
					MaxRetries:  3,
				},
			},
		},
		Embed: VectorEmbedConfig{
			BackstopInterval: "24h",
		},
	}
}

// mutateServer applies fn to the named server entry, working around map
// values not being addressable.
func mutateServer(c *VectorConfig, name string, fn func(*VectorEmbeddingsServerConfig)) {
	s := c.Embeddings.Servers[name]
	fn(&s)
	c.Embeddings.Servers[name] = s
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
			name:    "enabled zero max_input_chars",
			mutate:  func(c *VectorConfig) { c.Embeddings.MaxInputChars = 0 },
			wantErr: "max_input_chars",
		},
		{
			name:    "enabled negative max_input_chars",
			mutate:  func(c *VectorConfig) { c.Embeddings.MaxInputChars = -1 },
			wantErr: "max_input_chars",
		},
		{
			name:    "enabled with no servers",
			mutate:  func(c *VectorConfig) { c.Embeddings.Servers = nil },
			wantErr: "at least one server",
		},
		{
			name: "multiple servers without default_server",
			mutate: func(c *VectorConfig) {
				c.Embeddings.DefaultServer = ""
			},
			wantErr: "default_server is required",
		},
		{
			name: "default_server names an undefined server",
			mutate: func(c *VectorConfig) {
				c.Embeddings.DefaultServer = "nope"
			},
			wantErr: `default_server "nope" is not a defined server`,
		},
		{
			name: "single server needs no default_server",
			mutate: func(c *VectorConfig) {
				delete(c.Embeddings.Servers, "remote")
				c.Embeddings.DefaultServer = ""
			},
		},
		{
			name: "server missing endpoint",
			mutate: func(c *VectorConfig) {
				mutateServer(c, "local", func(s *VectorEmbeddingsServerConfig) { s.Endpoint = "" })
			},
			wantErr: "[vector.embeddings.servers.local] endpoint is required",
		},
		{
			name: "server zero batch_size",
			mutate: func(c *VectorConfig) {
				mutateServer(c, "local", func(s *VectorEmbeddingsServerConfig) { s.BatchSize = 0 })
			},
			wantErr: "batch_size",
		},
		{
			name: "server negative batch_size",
			mutate: func(c *VectorConfig) {
				mutateServer(c, "local", func(s *VectorEmbeddingsServerConfig) { s.BatchSize = -1 })
			},
			wantErr: "batch_size",
		},
		{
			name: "server zero concurrency",
			mutate: func(c *VectorConfig) {
				mutateServer(c, "remote", func(s *VectorEmbeddingsServerConfig) { s.Concurrency = 0 })
			},
			wantErr: "[vector.embeddings.servers.remote] concurrency",
		},
		{
			name: "server negative max_retries",
			mutate: func(c *VectorConfig) {
				mutateServer(c, "local", func(s *VectorEmbeddingsServerConfig) { s.MaxRetries = -1 })
			},
			wantErr: "max_retries",
		},
		{
			name: "server zero max_retries disables retries and is valid",
			mutate: func(c *VectorConfig) {
				mutateServer(c, "local", func(s *VectorEmbeddingsServerConfig) { s.MaxRetries = 0 })
			},
		},
		{
			name: "server bad timeout",
			mutate: func(c *VectorConfig) {
				mutateServer(c, "local", func(s *VectorEmbeddingsServerConfig) { s.Timeout = "not-a-duration" })
			},
			wantErr: "timeout",
		},
		{
			name: "server zero timeout",
			mutate: func(c *VectorConfig) {
				mutateServer(c, "local", func(s *VectorEmbeddingsServerConfig) { s.Timeout = "0s" })
			},
			wantErr: "timeout",
		},
		{
			name: "server negative timeout",
			mutate: func(c *VectorConfig) {
				mutateServer(c, "local", func(s *VectorEmbeddingsServerConfig) { s.Timeout = "-1s" })
			},
			wantErr: "timeout",
		},
		{
			name:    "enabled bad backstop interval",
			mutate:  func(c *VectorConfig) { c.Embed.BackstopInterval = "not-a-duration" },
			wantErr: "backstop_interval",
		},
		{
			name:    "enabled explicit zero backstop interval is invalid",
			mutate:  func(c *VectorConfig) { c.Embed.BackstopInterval = "0s" },
			wantErr: "use a negative value to disable",
		},
		{
			name:   "enabled negative backstop interval disables and is valid",
			mutate: func(c *VectorConfig) { c.Embed.BackstopInterval = "-1s" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validVectorConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestVectorEmbeddingsServerResolution(t *testing.T) {
	c := validVectorConfig().Embeddings

	name, server, err := c.Server("")
	require.NoError(t, err)
	assert.Equal(t, "local", name, "empty name resolves to default_server")
	assert.Equal(t, "http://localhost:11434/v1", server.Endpoint)

	name, server, err = c.Server("remote")
	require.NoError(t, err)
	assert.Equal(t, "remote", name)
	assert.Equal(t, "http://build-box:30000/v1", server.Endpoint)

	_, _, err = c.Server("nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no server named "nope"`)
	assert.Contains(t, err.Error(), "local, remote", "error lists the defined servers")

	c.DefaultServer = ""
	delete(c.Servers, "remote")
	name, _, err = c.Server("")
	require.NoError(t, err)
	assert.Equal(t, "local", name, "a single server is the implicit default")
}

func TestVectorConfigDefaults(t *testing.T) {
	cfg, err := Default()
	require.NoError(t, err)

	assert.Equal(t, 8192, cfg.Vector.Embeddings.MaxInputChars)
	assert.Empty(t, cfg.Vector.Embeddings.Servers)
	assert.True(t, cfg.Vector.Embed.RunAfterSyncEnabled(),
		"run_after_sync defaults to true when unset")
	assert.False(t, cfg.Vector.Embed.Recall,
		"automatic Recall embedding requires explicit opt-in")

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
	server := VectorEmbeddingsServerConfig{}
	assert.Equal(t, "", server.APIKey(), "no env var configured")

	server.APIKeyEnv = "AGENTSVIEW_TEST_VECTOR_API_KEY"
	assert.Equal(t, "", server.APIKey(), "configured env var not set in environment")

	t.Setenv("AGENTSVIEW_TEST_VECTOR_API_KEY", "secret-123")
	assert.Equal(t, "secret-123", server.APIKey())
}

// minimalServers returns the smallest valid servers table for TOML load
// tests, as the raw map shape loadMinimalWithConfig marshals.
func minimalServers() map[string]any {
	return map[string]any{
		"local": map[string]any{
			"endpoint": "http://localhost:11434/v1",
		},
	}
}

// TestVectorConfigTOMLLoad exercises the full config-file load path so the
// default-merge logic in applyConfigTOML (not just the section types in
// isolation) is covered, including per-server defaults and the ability to
// explicitly override a zero-value field like max_retries.
func TestVectorConfigTOMLLoad(t *testing.T) {
	t.Run("unset server fields keep defaults, explicit zero overrides", func(t *testing.T) {
		cfg := loadMinimalWithConfig(t, map[string]any{
			"vector": map[string]any{
				"enabled": true,
				"embeddings": map[string]any{
					"model":     "nomic-embed-text",
					"dimension": 768,
					"servers": map[string]any{
						"local": map[string]any{
							"endpoint":    "http://localhost:11434/v1",
							"max_retries": 0,
						},
					},
				},
			},
		})
		require.True(t, cfg.Vector.Enabled)
		server := cfg.Vector.Embeddings.Servers["local"]
		assert.Equal(t, "http://localhost:11434/v1", server.Endpoint)
		assert.Equal(t, 32, server.BatchSize, "unset batch_size keeps default")
		assert.Equal(t, 4, server.Concurrency, "unset concurrency keeps default")
		assert.Equal(t, "30s", server.Timeout, "unset timeout keeps default")
		assert.Equal(t, 0, server.MaxRetries, "explicit max_retries=0 overrides default")
		assert.Equal(t, 8192, cfg.Vector.Embeddings.MaxInputChars, "unset max_input_chars keeps default")
		assert.Equal(t, "24h", cfg.Vector.Embed.BackstopInterval, "unset backstop_interval keeps default")
		assert.False(t, cfg.Vector.IncludeAutomated, "unset include_automated keeps the false default")
		assert.Empty(t, cfg.Vector.Embeddings.QueryPrefix, "unset query_prefix defaults to empty")
		assert.Empty(t, cfg.Vector.Embeddings.DocumentPrefix,
			"unset document_prefix defaults to empty")
		assert.Empty(t, cfg.Vector.Embeddings.InputSuffix, "unset input_suffix defaults to empty")
		assert.False(t, cfg.Vector.Embeddings.RequestDimensions,
			"unset request_dimensions keeps the false default")
	})

	t.Run("role prefixes load verbatim", func(t *testing.T) {
		cfg := loadMinimalWithConfig(t, map[string]any{
			"vector": map[string]any{
				"enabled": true,
				"embeddings": map[string]any{
					"model":           "embeddinggemma",
					"dimension":       768,
					"query_prefix":    "task: search result | query: ",
					"document_prefix": "title: none | text: ",
					"servers":         minimalServers(),
				},
			},
		})
		assert.Equal(t, "task: search result | query: ",
			cfg.Vector.Embeddings.QueryPrefix)
		assert.Equal(t, "title: none | text: ",
			cfg.Vector.Embeddings.DocumentPrefix)
	})

	t.Run("request_dimensions true is loaded", func(t *testing.T) {
		cfg := loadMinimalWithConfig(t, map[string]any{
			"vector": map[string]any{
				"enabled": true,
				"embeddings": map[string]any{
					"model":              "qwen3-embedding:0.6b",
					"dimension":          256,
					"request_dimensions": true,
					"servers":            minimalServers(),
				},
			},
		})
		assert.True(t, cfg.Vector.Embeddings.RequestDimensions)
		assert.Equal(t, 256, cfg.Vector.Embeddings.Dimension)
	})

	t.Run("named servers with default_server load and resolve", func(t *testing.T) {
		cfg := loadMinimalWithConfig(t, map[string]any{
			"vector": map[string]any{
				"enabled": true,
				"embeddings": map[string]any{
					"model":          "qwen3-embedding-4b",
					"dimension":      2560,
					"input_suffix":   "<|endoftext|>",
					"default_server": "local",
					"servers": map[string]any{
						"local": map[string]any{
							"endpoint": "http://127.0.0.1:30000/v1",
						},
						"remote": map[string]any{
							"endpoint":    "http://build-box:30000/v1",
							"timeout":     "300s",
							"concurrency": 6,
						},
					},
				},
			},
		})
		assert.Equal(t, "<|endoftext|>", cfg.Vector.Embeddings.InputSuffix)
		assert.Equal(t, "local", cfg.Vector.Embeddings.DefaultServer)

		name, server, err := cfg.Vector.Embeddings.Server("")
		require.NoError(t, err)
		assert.Equal(t, "local", name)
		assert.Equal(t, "http://127.0.0.1:30000/v1", server.Endpoint)

		_, remote, err := cfg.Vector.Embeddings.Server("remote")
		require.NoError(t, err)
		assert.Equal(t, "300s", remote.Timeout, "per-server timeout override")
		assert.Equal(t, 6, remote.Concurrency, "per-server concurrency override")
		assert.Equal(t, 32, remote.BatchSize, "unset per-server batch_size keeps default")
	})

	t.Run("include_automated true is loaded", func(t *testing.T) {
		cfg := loadMinimalWithConfig(t, map[string]any{
			"vector": map[string]any{
				"enabled":           true,
				"include_automated": true,
				"embeddings": map[string]any{
					"model":     "nomic-embed-text",
					"dimension": 768,
					"servers":   minimalServers(),
				},
			},
		})
		assert.True(t, cfg.Vector.IncludeAutomated)
	})

	t.Run("automatic Recall embedding opt-in is loaded", func(t *testing.T) {
		cfg := loadMinimalWithConfig(t, map[string]any{
			"vector": map[string]any{
				"enabled": true,
				"embeddings": map[string]any{
					"model":     "nomic-embed-text",
					"dimension": 768,
					"servers":   minimalServers(),
				},
				"embed": map[string]any{
					"recall": true,
				},
			},
		})
		assert.True(t, cfg.Vector.Embed.Recall)
	})

	t.Run("enabled without servers fails to load", func(t *testing.T) {
		err := loadMinimalErrWithConfig(t, map[string]any{
			"vector": map[string]any{
				"enabled": true,
				"embeddings": map[string]any{
					"model":     "nomic-embed-text",
					"dimension": 768,
				},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one server")
	})

	t.Run("multiple servers without default_server fails to load", func(t *testing.T) {
		servers := minimalServers()
		servers["remote"] = map[string]any{"endpoint": "http://build-box:30000/v1"}
		err := loadMinimalErrWithConfig(t, map[string]any{
			"vector": map[string]any{
				"enabled": true,
				"embeddings": map[string]any{
					"model":     "nomic-embed-text",
					"dimension": 768,
					"servers":   servers,
				},
			},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "default_server is required")
	})

	t.Run("disabled section with no fields loads fine", func(t *testing.T) {
		cfg := loadMinimalWithConfig(t, map[string]any{
			"vector": map[string]any{},
		})
		assert.False(t, cfg.Vector.Enabled)
	})

	t.Run("explicit zero/negative operational overrides fail to load", func(t *testing.T) {
		tests := []struct {
			name    string
			server  map[string]any
			embed   map[string]any
			wantErr string
		}{
			{
				name:    "explicit zero batch_size",
				server:  map[string]any{"batch_size": 0},
				wantErr: "batch_size",
			},
			{
				name:    "explicit negative batch_size",
				server:  map[string]any{"batch_size": -1},
				wantErr: "batch_size",
			},
			{
				name:    "explicit zero concurrency",
				server:  map[string]any{"concurrency": 0},
				wantErr: "concurrency",
			},
			{
				name:    "explicit negative max_retries",
				server:  map[string]any{"max_retries": -1},
				wantErr: "max_retries",
			},
			{
				name:    "explicit zero timeout",
				server:  map[string]any{"timeout": "0s"},
				wantErr: "timeout",
			},
			{
				name:    "explicit zero backstop_interval",
				embed:   map[string]any{"backstop_interval": "0s"},
				wantErr: "use a negative value to disable",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				local := map[string]any{"endpoint": "http://localhost:11434/v1"}
				maps.Copy(local, tt.server)
				vector := map[string]any{
					"enabled": true,
					"embeddings": map[string]any{
						"model":     "nomic-embed-text",
						"dimension": 768,
						"servers":   map[string]any{"local": local},
					},
				}
				if tt.embed != nil {
					vector["embed"] = tt.embed
				}
				err := loadMinimalErrWithConfig(t, map[string]any{"vector": vector})
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			})
		}
	})
}
