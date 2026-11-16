package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

const fileSecret = "file-secret-value"

// secretDir returns an owner-only directory: secret files under a directory
// others can write are refused, and a umask of 002 leaves t.TempDir that way.
func secretDir(t *testing.T) string {
	t.Helper()
	dir := canonicalTempDir(t)
	require.NoError(t, os.Chmod(dir, 0o700))
	return dir
}

// writePrivateFile creates a file only the current user can read, which
// secret files must be on every platform.
func writePrivateFile(t *testing.T, path, contents string) {
	t.Helper()
	file, err := safefileio.CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = file.WriteString(contents)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

// writeSecretFile writes a private secret file with a trailing newline, as
// `echo value > file` would.
func writeSecretFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(secretDir(t), "secret")
	writePrivateFile(t, path, fileSecret+"\n")
	return path
}

func TestSecretFileSettingsLoadFromConfig(t *testing.T) {
	tests := []struct {
		name   string
		config func(path string) map[string]any
		secret func(t *testing.T, cfg Config) string
	}{
		{
			name: "insights",
			config: func(path string) map[string]any {
				return map[string]any{"insights": map[string]any{
					"endpoint":     "https://models.example.test/v1",
					"model":        "m",
					"api_key_file": path,
				}}
			},
			secret: func(_ *testing.T, cfg Config) string { return cfg.Insights.APIKey() },
		},
		{
			name: "docbank",
			config: func(path string) map[string]any {
				return map[string]any{"integrations": map[string]any{"docbank": map[string]any{
					"url":          "https://docbank.example.test",
					"api_key_file": path,
				}}}
			},
			secret: func(t *testing.T, cfg Config) string {
				assert.True(t, cfg.Integrations.Docbank.Enabled())
				return cfg.Integrations.Docbank.APIKey()
			},
		},
		{
			name: "kata",
			config: func(path string) map[string]any {
				return map[string]any{"kata": map[string]any{
					"enabled":    true,
					"endpoint":   "https://kata.example.test",
					"token_file": path,
				}}
			},
			secret: func(_ *testing.T, cfg Config) string { return cfg.Kata.FileToken() },
		},
		{
			name: "embeddings server",
			config: func(path string) map[string]any {
				return map[string]any{"vector": map[string]any{
					"enabled": true,
					"embeddings": map[string]any{
						"model":     "nomic-embed-text",
						"dimension": 768,
						"servers": map[string]any{"remote": map[string]any{
							"endpoint":     "https://embed.example.test/v1",
							"api_key_file": path,
						}},
					},
				}}
			},
			secret: func(_ *testing.T, cfg Config) string {
				return cfg.Vector.Embeddings.Servers["remote"].APIKey()
			},
		},
		{
			name: "recall extract server",
			config: func(path string) map[string]any {
				return map[string]any{"recall": map[string]any{"extract": map[string]any{
					"enabled": true,
					"model":   "m",
					"servers": map[string]any{"remote": map[string]any{
						"endpoint":     "https://extract.example.test/v1",
						"api_key_file": path,
					}},
				}}}
			},
			secret: func(_ *testing.T, cfg Config) string {
				return cfg.Recall.Extract.Servers["remote"].APIKey()
			},
		},
		{
			name: "duckdb token",
			config: func(path string) map[string]any {
				return map[string]any{"duckdb": map[string]any{
					"url":        "quack://duck.example.test",
					"token_file": path,
				}}
			},
			secret: func(t *testing.T, cfg Config) string {
				duck, err := cfg.ResolveDuckDB()
				require.NoError(t, err)
				return duck.Token
			},
		},
		{
			name: "cursor admin api key",
			config: func(path string) map[string]any {
				return map[string]any{"cursor_admin_api_key_file": path}
			},
			secret: func(t *testing.T, cfg Config) string {
				key, err := cfg.ResolveCursorAdminAPIKey()
				require.NoError(t, err)
				return key
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadMinimalWithConfig(t, tt.config(writeSecretFile(t)))
			assert.Equal(t, fileSecret, tt.secret(t, cfg),
				"the file's contents, minus the trailing newline")
		})
	}
}

func TestSecretFileSettingsRejectUnusableFiles(t *testing.T) {
	insights := func(path string) map[string]any {
		return map[string]any{"insights": map[string]any{
			"endpoint":     "https://models.example.test/v1",
			"model":        "m",
			"api_key_file": path,
		}}
	}

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(secretDir(t), "absent")
		err := loadMinimalErrWithConfig(t, insights(path))
		requireErrorContains(t, err, "[insights] api_key_file", path)
	})

	t.Run("empty file", func(t *testing.T) {
		path := filepath.Join(secretDir(t), "empty")
		writePrivateFile(t, path, "\n")
		err := loadMinimalErrWithConfig(t, insights(path))
		requireErrorContains(t, err, "[insights] api_key_file", path)
	})

	t.Run("file others can read", func(t *testing.T) {
		skipIfNotUnix(t)
		path := filepath.Join(secretDir(t), "shared")
		writePrivateFile(t, path, fileSecret)
		require.NoError(t, os.Chmod(path, 0o644))
		err := loadMinimalErrWithConfig(t, insights(path))
		requireErrorContains(t, err, "[insights] api_key_file", path)
		assert.NotContains(t, err.Error(), fileSecret, "errors never show the secret")
	})

	t.Run("commands that do not use the section only warn", func(t *testing.T) {
		f := newConfigFixture(t)
		f.WriteTOML(t, insights(filepath.Join(f.Dir, "absent")))
		cfg, err := LoadMinimalFor(ScopeLocal)
		require.NoError(t, err)
		assert.Empty(t, cfg.Insights.APIKey())
	})
}

func TestSecretFileSettingsRejectTwoSources(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]any
		wantErr string
	}{
		{
			name: "insights",
			config: map[string]any{"insights": map[string]any{
				"endpoint": "https://models.example.test/v1", "model": "m",
				"api_key_env": "KEY", "api_key_file": "/run/secrets/key",
			}},
			wantErr: "[insights] set only one of api_key_env and api_key_file",
		},
		{
			name: "docbank",
			config: map[string]any{"integrations": map[string]any{"docbank": map[string]any{
				"url": "https://docbank.example.test", "api_key_env": "KEY", "api_key_file": "/run/secrets/key",
			}}},
			wantErr: "[integrations.docbank] set only one of api_key_env and api_key_file",
		},
		{
			name: "kata",
			config: map[string]any{"kata": map[string]any{
				"enabled": true, "endpoint": "https://kata.example.test",
				"token_env": "KEY", "token_file": "/run/secrets/key",
			}},
			wantErr: "[kata] set only one of token_env and token_file",
		},
		{
			name: "embeddings server",
			config: map[string]any{"vector": map[string]any{
				"enabled": true,
				"embeddings": map[string]any{
					"model": "nomic-embed-text", "dimension": 768,
					"servers": map[string]any{"remote": map[string]any{
						"endpoint":    "https://embed.example.test/v1",
						"api_key_env": "KEY", "api_key_file": "/run/secrets/key",
					}},
				},
			}},
			wantErr: "[vector.embeddings.servers.remote] set only one of api_key_env and api_key_file",
		},
		{
			name: "recall extract server",
			config: map[string]any{"recall": map[string]any{"extract": map[string]any{
				"enabled": true, "model": "m",
				"servers": map[string]any{"remote": map[string]any{
					"endpoint":    "https://extract.example.test/v1",
					"api_key_env": "KEY", "api_key_file": "/run/secrets/key",
				}},
			}}},
			wantErr: "[recall.extract.servers.remote] set only one of api_key_env and api_key_file",
		},
		{
			name: "duckdb token",
			config: map[string]any{"duckdb": map[string]any{
				"token": "inline", "token_file": "/run/secrets/key",
			}},
			wantErr: "[duckdb] set only one of token and token_file",
		},
		{
			name: "cursor admin api key",
			config: map[string]any{
				"cursor_admin_api_key": "inline", "cursor_admin_api_key_file": "/run/secrets/key",
			},
			wantErr: "set only one of cursor_admin_api_key and cursor_admin_api_key_file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadMinimalErrWithConfig(t, tt.config)
			requireErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestSecretFileEnvironmentOverridesReplaceFiles(t *testing.T) {
	t.Run("AGENTSVIEW_DUCKDB_TOKEN", func(t *testing.T) {
		f := newConfigFixture(t)
		t.Setenv("AGENTSVIEW_DUCKDB_TOKEN", "env-token")
		f.WriteTOML(t, map[string]any{"duckdb": map[string]any{
			"token_file": filepath.Join(f.Dir, "absent"),
		}})
		cfg := f.LoadMinimal(t)
		duck, err := cfg.ResolveDuckDB()
		require.NoError(t, err)
		assert.Equal(t, "env-token", duck.Token)
	})

	t.Run("AGENTSVIEW_CURSOR_ADMIN_API_KEY", func(t *testing.T) {
		f := newConfigFixture(t)
		t.Setenv("AGENTSVIEW_CURSOR_ADMIN_API_KEY", "env-key")
		f.WriteTOML(t, map[string]any{
			"cursor_admin_api_key_file": filepath.Join(f.Dir, "absent"),
		})
		cfg := f.LoadMinimal(t)
		key, err := cfg.ResolveCursorAdminAPIKey()
		require.NoError(t, err)
		assert.Equal(t, "env-key", key)
	})

	t.Run("unreadable duckdb token_file fails when resolved", func(t *testing.T) {
		f := newConfigFixture(t)
		path := filepath.Join(f.Dir, "absent")
		f.WriteTOML(t, map[string]any{"duckdb": map[string]any{"token_file": path}})
		cfg := f.LoadMinimal(t)
		_, err := cfg.ResolveDuckDB()
		requireErrorContains(t, err, "[duckdb] token_file", path)
	})
}
