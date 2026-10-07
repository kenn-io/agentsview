package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var deploymentTestEnv = []string{
	"AGENTSVIEW_MODE",
	"PG_SERVE",
	"AGENTSVIEW_HOST",
	"AGENTSVIEW_REQUIRE_AUTH",
	"AGENTSVIEW_NO_BROWSER",
	"AGENTSVIEW_PG_ALLOW_INSECURE",
	"AGENTSVIEW_AUTH_TOKEN",
	"AGENTSVIEW_AUTH_TOKEN_FILE",
	"AGENTSVIEW_EMBEDDINGS_ENDPOINT",
	"AGENTSVIEW_EMBEDDINGS_API_KEY_FILE",
	"AGENTSVIEW_EMBEDDINGS_BATCH_SIZE",
}

// unsetDeploymentEnv removes every deployment variable for the test; the
// host environment is restored afterwards.
func unsetDeploymentEnv(t *testing.T) {
	t.Helper()
	for _, name := range deploymentTestEnv {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
}

func writeSecret(t *testing.T, dir, name, value string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
	return path
}

type deploymentLoader struct {
	name   string
	remote bool
	load   func(args ...string) (Config, error)
}

// deploymentLoaders covers every public loader that layers deployment env.
func deploymentLoaders() []deploymentLoader {
	return []deploymentLoader{
		{name: "Load", load: func(args ...string) (Config, error) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			RegisterServeFlags(fs)
			if err := fs.Parse(args); err != nil {
				return Config{}, err
			}
			return Load(fs)
		}},
		{name: "LoadPFlags", load: func(args ...string) (Config, error) {
			fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
			RegisterServePFlags(fs)
			if err := fs.Parse(args); err != nil {
				return Config{}, err
			}
			return LoadPFlags(fs)
		}},
		{name: "LoadRemoteServePFlags", remote: true, load: func(args ...string) (Config, error) {
			fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
			RegisterServePFlags(fs)
			if err := fs.Parse(args); err != nil {
				return Config{}, err
			}
			return LoadRemoteServePFlags(fs)
		}},
		{name: "LoadMinimal", load: func(args ...string) (Config, error) {
			return LoadMinimal()
		}},
		{name: "LoadReadOnly", load: func(args ...string) (Config, error) {
			return LoadReadOnly()
		}},
	}
}

func TestDeploymentEnvIgnoredWithoutMode(t *testing.T) {
	for _, loader := range deploymentLoaders() {
		for _, mode := range []struct {
			name  string
			value *string
		}{
			{name: "unset"},
			{name: "blank", value: new("  ")},
		} {
			t.Run(loader.name+"/"+mode.name, func(t *testing.T) {
				unsetDeploymentEnv(t)
				dir := setupTestEnv(t)
				writeConfig(t, dir, map[string]any{
					"host":       "127.0.0.1",
					"auth_token": "file-token",
					"pg":         map[string]any{"url": "postgres://db.example/agentsview"},
				})
				want, err := loader.load()
				require.NoError(t, err)

				if mode.value != nil {
					t.Setenv("AGENTSVIEW_MODE", *mode.value)
				}
				t.Setenv("PG_SERVE", "on")
				t.Setenv("AGENTSVIEW_HOST", "192.0.2.10")
				t.Setenv("AGENTSVIEW_REQUIRE_AUTH", "maybe")
				t.Setenv("AGENTSVIEW_NO_BROWSER", "maybe")
				t.Setenv("AGENTSVIEW_PG_ALLOW_INSECURE", "maybe")
				t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", "/nonexistent/x")
				t.Setenv("AGENTSVIEW_EMBEDDINGS_ENDPOINT", " ")
				t.Setenv("AGENTSVIEW_EMBEDDINGS_API_KEY_FILE", "/nonexistent/key")

				got, err := loader.load()
				require.NoError(t, err)
				assert.Equal(t, want, got)
				wantPG, err := want.ResolvePG()
				require.NoError(t, err)
				gotPG, err := got.ResolvePG()
				require.NoError(t, err)
				assert.Equal(t, wantPG, gotPG)
			})
		}
	}
}

func TestDeploymentEnvAppliesUnderMode(t *testing.T) {
	for _, loader := range deploymentLoaders() {
		t.Run(loader.name, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			writeConfig(t, dir, map[string]any{
				"host":         "10.0.0.5",
				"require_auth": false,
			})
			t.Setenv("AGENTSVIEW_MODE", "serve")

			// File beats defaults when the variable is unset.
			cfg, err := loader.load()
			require.NoError(t, err)
			if loader.remote {
				assert.Equal(t, "127.0.0.1", cfg.Host, "remote serve isolates config.toml host")
			} else {
				assert.Equal(t, "10.0.0.5", cfg.Host)
			}
			assert.False(t, cfg.RequireAuth)

			// Env beats file.
			t.Setenv("AGENTSVIEW_HOST", "0.0.0.0")
			t.Setenv("AGENTSVIEW_REQUIRE_AUTH", "true")
			t.Setenv("AGENTSVIEW_NO_BROWSER", "true")
			cfg, err = loader.load()
			require.NoError(t, err)
			assert.Equal(t, "0.0.0.0", cfg.Host)
			assert.True(t, cfg.RequireAuth)
			assert.True(t, cfg.NoBrowser)
			assert.False(t, cfg.HostExplicit, "env must never mark the host explicit")
			assert.False(t, cfg.PortExplicit)

			if loader.name == "LoadMinimal" || loader.name == "LoadReadOnly" {
				return
			}
			// Flags beat env.
			flags := []string{"--host", "127.0.0.2", "--no-browser=false", "--require-auth=false", "--port", "9000"}
			if loader.name == "Load" {
				flags = []string{"-host", "127.0.0.2", "-no-browser=false", "-require-auth=false", "-port", "9000"}
			}
			cfg, err = loader.load(flags...)
			require.NoError(t, err)
			assert.Equal(t, "127.0.0.2", cfg.Host)
			assert.True(t, cfg.HostExplicit)
			assert.False(t, cfg.NoBrowser)
			assert.False(t, cfg.RequireAuth)
			assert.Equal(t, 9000, cfg.Port)
		})
	}
}

func TestDeploymentEnvRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct{ name, variable, value, want string }{
		{name: "mode", variable: "AGENTSVIEW_MODE", value: "daemon", want: "AGENTSVIEW_MODE"},
		{name: "require auth", variable: "AGENTSVIEW_REQUIRE_AUTH", value: "maybe", want: "AGENTSVIEW_REQUIRE_AUTH must be a boolean"},
		{name: "no browser", variable: "AGENTSVIEW_NO_BROWSER", value: "maybe", want: "AGENTSVIEW_NO_BROWSER must be a boolean"},
		{name: "pg allow insecure", variable: "AGENTSVIEW_PG_ALLOW_INSECURE", value: "maybe", want: "AGENTSVIEW_PG_ALLOW_INSECURE must be a boolean"},
		{name: "blank host", variable: "AGENTSVIEW_HOST", value: " ", want: "AGENTSVIEW_HOST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetDeploymentEnv(t)
			setupTestEnv(t)
			t.Setenv("AGENTSVIEW_MODE", "serve")
			t.Setenv(tc.variable, tc.value)
			_, err := LoadMinimal()
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestDeploymentMode(t *testing.T) {
	for _, tc := range []struct {
		name, mode, pgServe, want string
		set                       bool
	}{
		{name: "unset"},
		{name: "blank", mode: " \t"},
		{name: "pg serve ignored without mode", pgServe: "1"},
		{name: "serve", mode: "serve", want: DeploymentModeServe, set: true},
		{name: "pg-serve", mode: "pg-serve", want: DeploymentModePGServe, set: true},
		{name: "pg serve selector", mode: "serve", pgServe: "ON", want: DeploymentModePGServe, set: true},
		{name: "pg serve falsy", mode: "serve", pgServe: "0", want: DeploymentModeServe, set: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetDeploymentEnv(t)
			if tc.mode != "" {
				t.Setenv("AGENTSVIEW_MODE", tc.mode)
			}
			if tc.pgServe != "" {
				t.Setenv("PG_SERVE", tc.pgServe)
			}
			mode, set, err := DeploymentMode()
			require.NoError(t, err)
			assert.Equal(t, tc.want, mode)
			assert.Equal(t, tc.set, set)
		})
	}
}

func TestDeploymentAuthTokenFile(t *testing.T) {
	setup := func(t *testing.T) (dir string, configBytes []byte) {
		t.Helper()
		unsetDeploymentEnv(t)
		dir = setupTestEnv(t)
		writeConfig(t, dir, map[string]any{"auth_token": "file-token"})
		data, err := os.ReadFile(filepath.Join(dir, configFileName))
		require.NoError(t, err)
		t.Setenv("AGENTSVIEW_MODE", "serve")
		return dir, data
	}

	t.Run("overrides config.toml", func(t *testing.T) {
		dir, _ := setup(t)
		t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE",
			writeSecret(t, t.TempDir(), "token", "secret-from-file\n"))
		cfg, err := LoadMinimal()
		require.NoError(t, err)
		assert.Equal(t, "secret-from-file", cfg.AuthToken)
		require.NoError(t, cfg.EnsureAuthToken())
		assert.Equal(t, "secret-from-file", cfg.AuthToken)
		data, err := os.ReadFile(filepath.Join(dir, configFileName))
		require.NoError(t, err)
		assert.Contains(t, string(data), "file-token", "the file token stays on disk")
	})

	t.Run("expands home", func(t *testing.T) {
		setup(t)
		home := t.TempDir()
		setTestHome(t, home)
		writeSecret(t, home, "token", "home-secret")
		t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", "~/token")
		cfg, err := LoadMinimal()
		require.NoError(t, err)
		assert.Equal(t, "home-secret", cfg.AuthToken)
	})

	for _, tc := range []struct{ name, content, want string }{
		{name: "empty path", want: "AGENTSVIEW_AUTH_TOKEN_FILE must name a file"},
		{name: "missing", want: "AGENTSVIEW_AUTH_TOKEN_FILE"},
		{name: "blank", content: " \n\t", want: "AGENTSVIEW_AUTH_TOKEN_FILE"},
	} {
		t.Run(tc.name+" fails closed", func(t *testing.T) {
			dir, before := setup(t)
			path := filepath.Join(t.TempDir(), "token")
			if tc.name == "empty path" {
				path = ""
			} else if tc.name != "missing" {
				path = writeSecret(t, filepath.Dir(path), "token", tc.content)
			}
			t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", path)
			for _, load := range []func() (Config, error){LoadMinimal, LoadReadOnly} {
				_, err := load()
				require.ErrorContains(t, err, tc.want)
				assert.NotContains(t, err.Error(), "file-token")
			}
			after, err := os.ReadFile(filepath.Join(dir, configFileName))
			require.NoError(t, err)
			assert.Equal(t, before, after, "a failed secret read leaves config.toml unchanged")
		})
	}

	t.Run("both selectors fail", func(t *testing.T) {
		setup(t)
		t.Setenv("AGENTSVIEW_AUTH_TOKEN", "env-token")
		t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE",
			writeSecret(t, t.TempDir(), "token", "secret-from-file"))
		_, err := LoadMinimal()
		require.ErrorContains(t, err, "set only one")
		assert.NotContains(t, err.Error(), "env-token")
		assert.NotContains(t, err.Error(), "secret-from-file")
	})
}

func TestDeploymentInlineSecretsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		env    map[string]string
	}{
		{name: "config auth token", config: map[string]any{"auth_token": "file-token"}},
		{
			name:   "env auth token",
			config: map[string]any{"auth_token": "file-token"},
			env:    map[string]string{"AGENTSVIEW_AUTH_TOKEN": "env-token"},
		},
		{name: "pg url password", config: map[string]any{
			"pg": map[string]any{"url": "postgres://user:inline-pass@db.example/agentsview"},
		}},
		{name: "env pg url with expansion", env: map[string]string{
			"AGENTSVIEW_PG_URL":   "postgres://user:${DEPLOY_TEST_PG_PASS}@db.example/agentsview",
			"DEPLOY_TEST_PG_PASS": "expanded-pass",
		}},
		{name: "embedding api_key_env", config: map[string]any{
			"vector": map[string]any{
				"enabled": true,
				"embeddings": map[string]any{
					"model": "m", "dimension": 4,
					"servers": map[string]any{"local": map[string]any{
						"endpoint": "http://127.0.0.1:1/v1", "api_key_env": "DEPLOY_TEST_EMBED_KEY",
					}},
				},
			},
		}, env: map[string]string{"DEPLOY_TEST_EMBED_KEY": "inline-embed-key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			if tc.config != nil {
				writeConfig(t, dir, tc.config)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			want, err := LoadMinimal()
			require.NoError(t, err)
			t.Setenv("AGENTSVIEW_MODE", "serve")
			got, err := LoadMinimal()
			require.NoError(t, err)

			assert.Equal(t, want, got)
			assert.Equal(t, want.AuthToken, got.AuthToken)
			wantPG, err := want.ResolvePG()
			require.NoError(t, err)
			gotPG, err := got.ResolvePG()
			require.NoError(t, err)
			assert.Equal(t, wantPG.URL, gotPG.URL)
			if server, ok := got.Vector.Embeddings.Servers["local"]; ok {
				assert.Equal(t, "inline-embed-key", server.APIKey())
			}
		})
	}
}

func TestDeploymentEmbeddings(t *testing.T) {
	t.Run("endpoint gets owner defaults", func(t *testing.T) {
		unsetDeploymentEnv(t)
		setupTestEnv(t)
		t.Setenv("AGENTSVIEW_MODE", "pg-serve")
		t.Setenv("AGENTSVIEW_EMBEDDINGS_ENDPOINT", " http://embeddings.internal/v1 ")
		cfg, err := LoadMinimal()
		require.NoError(t, err)
		require.NotNil(t, cfg.DeploymentEmbeddings)
		assert.Equal(t, "http://embeddings.internal/v1", cfg.DeploymentEmbeddings.Endpoint)
		assert.Equal(t, 32, cfg.DeploymentEmbeddings.BatchSize)
		assert.Equal(t, 4, cfg.DeploymentEmbeddings.Concurrency)
		assert.Equal(t, "30s", cfg.DeploymentEmbeddings.Timeout)
		assert.Equal(t, 3, cfg.DeploymentEmbeddings.MaxRetries)
		assert.Empty(t, cfg.DeploymentEmbeddings.APIKey())
		assert.False(t, cfg.Vector.Enabled, "the endpoint alone never enables [vector]")
	})

	t.Run("custom batch size", func(t *testing.T) {
		unsetDeploymentEnv(t)
		setupTestEnv(t)
		t.Setenv("AGENTSVIEW_MODE", "pg-serve")
		t.Setenv("AGENTSVIEW_EMBEDDINGS_ENDPOINT", "http://embeddings.example/v1")
		t.Setenv("AGENTSVIEW_EMBEDDINGS_BATCH_SIZE", " 2 ")
		cfg, err := LoadMinimal()
		require.NoError(t, err)
		require.NotNil(t, cfg.DeploymentEmbeddings)
		assert.Equal(t, 2, cfg.DeploymentEmbeddings.BatchSize)
	})

	t.Run("key file expands home", func(t *testing.T) {
		unsetDeploymentEnv(t)
		setupTestEnv(t)
		home := t.TempDir()
		setTestHome(t, home)
		writeSecret(t, home, "embed-key", "embed-secret\n")
		t.Setenv("AGENTSVIEW_MODE", "serve")
		t.Setenv("AGENTSVIEW_EMBEDDINGS_ENDPOINT", "http://embeddings.internal/v1")
		t.Setenv("AGENTSVIEW_EMBEDDINGS_API_KEY_FILE", "~/embed-key")
		cfg, err := LoadMinimal()
		require.NoError(t, err)
		require.NotNil(t, cfg.DeploymentEmbeddings)
		assert.Equal(t, "embed-secret", cfg.DeploymentEmbeddings.APIKey())
	})
}

func TestDeploymentEmbeddingsRejectInvalid(t *testing.T) {
	for _, tc := range []struct {
		name      string
		vector    bool
		endpoint  *string
		keyFile   string
		batchSize *string
		want      string
	}{
		{name: "with vector section", vector: true, endpoint: new("http://e/v1"), want: "AGENTSVIEW_EMBEDDINGS_*"},
		{name: "key file without endpoint", keyFile: "key-secret", want: "AGENTSVIEW_EMBEDDINGS_ENDPOINT"},
		{name: "blank endpoint", endpoint: new("  "), want: "AGENTSVIEW_EMBEDDINGS_ENDPOINT"},
		{name: "blank key file", endpoint: new("http://e/v1"), keyFile: " \n", want: "AGENTSVIEW_EMBEDDINGS_API_KEY_FILE"},
		{name: "missing key file", endpoint: new("http://e/v1"), keyFile: "missing", want: "AGENTSVIEW_EMBEDDINGS_API_KEY_FILE"},
		{name: "batch size without endpoint", batchSize: new("2"), want: "AGENTSVIEW_EMBEDDINGS_ENDPOINT"},
		{name: "batch size with vector section", vector: true, batchSize: new("2"), want: "AGENTSVIEW_EMBEDDINGS_*"},
		{name: "non-integer batch size", endpoint: new("http://e/v1"), batchSize: new("1.5"), want: "AGENTSVIEW_EMBEDDINGS_BATCH_SIZE"},
		{name: "blank batch size", endpoint: new("http://e/v1"), batchSize: new(" "), want: "AGENTSVIEW_EMBEDDINGS_BATCH_SIZE"},
		{name: "zero batch size", endpoint: new("http://e/v1"), batchSize: new("0"), want: "AGENTSVIEW_EMBEDDINGS_BATCH_SIZE"},
		{name: "negative batch size", endpoint: new("http://e/v1"), batchSize: new("-1"), want: "AGENTSVIEW_EMBEDDINGS_BATCH_SIZE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			if tc.vector {
				writeConfig(t, dir, map[string]any{"vector": map[string]any{"enabled": false}})
			}
			t.Setenv("AGENTSVIEW_MODE", "serve")
			if tc.endpoint != nil {
				t.Setenv("AGENTSVIEW_EMBEDDINGS_ENDPOINT", *tc.endpoint)
			}
			if tc.batchSize != nil {
				t.Setenv("AGENTSVIEW_EMBEDDINGS_BATCH_SIZE", *tc.batchSize)
			}
			switch tc.keyFile {
			case "":
			case "missing":
				t.Setenv("AGENTSVIEW_EMBEDDINGS_API_KEY_FILE", filepath.Join(t.TempDir(), "absent"))
			default:
				t.Setenv("AGENTSVIEW_EMBEDDINGS_API_KEY_FILE", writeSecret(t, t.TempDir(), "key", tc.keyFile))
			}
			_, err := LoadMinimal()
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), "key-secret")
		})
	}
}

func TestDeploymentPGAllowInsecure(t *testing.T) {
	t.Run("legacy default target", func(t *testing.T) {
		for _, tc := range []struct {
			name, value string
			file, want  bool
		}{
			{name: "enables", value: "1", want: true},
			{name: "disables file setting", value: "false", file: true, want: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				unsetDeploymentEnv(t)
				dir := setupTestEnv(t)
				writeConfig(t, dir, map[string]any{"pg": map[string]any{
					"url": "postgres://db.example/agentsview", "allow_insecure": tc.file,
				}})
				t.Setenv("AGENTSVIEW_MODE", "serve")
				t.Setenv("AGENTSVIEW_PG_ALLOW_INSECURE", tc.value)
				cfg, err := LoadMinimal()
				require.NoError(t, err)
				pg, err := cfg.ResolvePG()
				require.NoError(t, err)
				assert.Equal(t, tc.want, pg.AllowInsecure)
			})
		}
	})

	t.Run("named targets other than the default are untouched", func(t *testing.T) {
		unsetDeploymentEnv(t)
		dir := setupTestEnv(t)
		require.NoError(t, os.WriteFile(filepath.Join(dir, configFileName), []byte(`
default_pg = "hub"

[pg.hub]
url = "postgres://hub.example/agentsview"

[pg.other]
url = "postgres://other.example/agentsview"
`), 0o600))
		t.Setenv("AGENTSVIEW_MODE", "serve")
		t.Setenv("AGENTSVIEW_PG_ALLOW_INSECURE", "true")
		cfg, err := LoadMinimal()
		require.NoError(t, err)
		hub, err := cfg.ResolvePGTarget("hub")
		require.NoError(t, err)
		assert.True(t, hub.AllowInsecure)
		other, err := cfg.ResolvePGTarget("other")
		require.NoError(t, err)
		assert.False(t, other.AllowInsecure)
	})

	t.Run("unset keeps the file setting", func(t *testing.T) {
		unsetDeploymentEnv(t)
		dir := setupTestEnv(t)
		writeConfig(t, dir, map[string]any{"pg": map[string]any{
			"url": "postgres://db.example/agentsview", "allow_insecure": true,
		}})
		t.Setenv("AGENTSVIEW_MODE", "serve")
		cfg, err := LoadMinimal()
		require.NoError(t, err)
		pg, err := cfg.ResolvePG()
		require.NoError(t, err)
		assert.True(t, pg.AllowInsecure)
	})
}
