package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopedLoadersValidateUsedSections(t *testing.T) {
	for _, tt := range []struct {
		data, want string
		scope      ValidationScope
	}{
		{"[vector]\nenabled=true", "model is required", ScopeVector},
		{"[recall.extract]\nenabled=true\nmodel='local'", "recall.extract", ScopeRecall},
		{"[insights]\nendpoint='http://models.example/v1'\nmodel='remote'", "plaintext", ScopeInsights},
	} {
		t.Run(tt.want, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tt.data), 0o600))
			_, err := LoadMinimalFor(tt.scope)
			require.ErrorContains(t, err, tt.want)
			_, err = LoadReadOnlyFor(tt.scope)
			require.ErrorContains(t, err, tt.want)
			_, err = LoadPFlagsFor(pflag.NewFlagSet("test", pflag.ContinueOnError), tt.scope)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestScopedRemoteErrorsCannotFallBackToDefaults(t *testing.T) {
	unsetDeploymentEnv(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://db.example/override")
	t.Setenv("AGENTSVIEW_CLICKHOUSE_URL", "clickhouse://db.example/override")
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{
		"pg":         map[string]any{"all": map[string]any{"url": "postgres://db.example/archive"}},
		"clickhouse": map[string]any{"all": map[string]any{"url": "clickhouse://db.example/archive"}},
	})
	cfg, err := LoadMinimalFor(ScopeLocal)
	require.NoError(t, err)
	_, err = cfg.ResolvePG()
	require.ErrorContains(t, err, "reserved")
	_, err = cfg.ResolveClickHouse()
	require.ErrorContains(t, err, "reserved")
	_, _, err = cfg.PGTargetNames()
	require.ErrorContains(t, err, "reserved")
	_, _, err = cfg.ClickHouseTargetNames()
	require.ErrorContains(t, err, "reserved")
	_, err = cfg.DefaultPGTargetName()
	require.ErrorContains(t, err, "reserved")
	_, err = cfg.DefaultClickHouseTargetName()
	require.ErrorContains(t, err, "reserved")
	_, err = cfg.ResolvePGTargets()
	require.ErrorContains(t, err, "reserved")
	_, err = cfg.ResolveClickHouseTargets()
	require.ErrorContains(t, err, "reserved")
	for _, name := range []string{"", "other"} {
		_, err = cfg.RawPGTarget(name)
		require.ErrorContains(t, err, "reserved")
		_, err = cfg.RawClickHouseTarget(name)
		require.ErrorContains(t, err, "reserved")
		_, err = cfg.ResolvePGTarget(name)
		require.ErrorContains(t, err, "reserved")
		_, err = cfg.ResolveClickHouseTarget(name)
		assert.ErrorContains(t, err, "reserved")
	}
}

func TestScopedLoadersIgnoreUnusedDeploymentEnvironment(t *testing.T) {
	unsetDeploymentEnv(t)
	setupTestEnv(t)
	t.Setenv("AGENTSVIEW_MODE", "invalid")
	t.Setenv("AGENTSVIEW_REQUIRE_AUTH", "invalid")
	t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", "/nonexistent/scoped-config-token")
	for _, load := range []func() (Config, error){
		func() (Config, error) { return LoadMinimalFor(ScopeLocal) },
		func() (Config, error) { return LoadReadOnlyFor(ScopeLocal) },
		func() (Config, error) {
			return LoadPFlagsFor(pflag.NewFlagSet("test", pflag.ContinueOnError), ScopeLocal)
		},
	} {
		_, err := load()
		require.NoError(t, err)
	}
	_, err := LoadMinimalFor(ScopePG | ScopeDeployment)
	require.ErrorContains(t, err, "AGENTSVIEW_MODE")
}

func TestScopedPGConnectionDeploymentOverride(t *testing.T) {
	for name, load := range map[string]func() (Config, error){
		"minimal":  func() (Config, error) { return LoadMinimalFor(ScopePG) },
		"readonly": func() (Config, error) { return LoadReadOnlyFor(ScopeLocal) },
		"flags": func() (Config, error) {
			return LoadPFlagsFor(pflag.NewFlagSet("test", pflag.ContinueOnError), ScopeLocal)
		},
	} {
		t.Run(name, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			writeConfig(t, dir, map[string]any{"pg": map[string]any{"url": "postgres://db.example/archive"}})
			t.Setenv("AGENTSVIEW_MODE", "serve")
			t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", filepath.Join(dir, "missing-token"))
			for _, value := range []string{"true", "false", "invalid"} {
				t.Setenv("AGENTSVIEW_PG_ALLOW_INSECURE", value)
				cfg, err := load()
				require.NoError(t, err, "loading must not read unrelated deployment token files")
				pg, err := cfg.ResolvePG()
				if value == "invalid" {
					require.ErrorContains(t, err, "AGENTSVIEW_PG_ALLOW_INSECURE must be a boolean")
				} else {
					require.NoError(t, err)
					assert.Equal(t, value == "true", pg.AllowInsecure)
				}
			}
		})
	}
}

// A mistyped AGENTSVIEW_MODE makes serve refuse to start. Other commands must
// not apply deployment secrets or the PG insecure override under it: a reader
// reports the mode once one of its variables is set, and is a no-op otherwise.
func TestInvalidDeploymentModeAppliesNoDeploymentSettings(t *testing.T) {
	for _, tt := range []struct {
		name, env, value string
		// apply reports whether the deployment value took effect.
		apply func(cfg *Config) (bool, error)
		// validate replaces apply for a reader whose error is its only
		// observable result.
		validate func(cfg *Config) error
	}{
		{name: "auth token file", env: "AGENTSVIEW_AUTH_TOKEN_FILE", value: "deployment-secret", apply: func(cfg *Config) (bool, error) {
			err := cfg.ApplyDeploymentAuth()
			return cfg.AuthToken != "", err
		}},
		{name: "embeddings", env: "AGENTSVIEW_EMBEDDINGS_ENDPOINT", value: "http://127.0.0.1:30000/v1", apply: func(cfg *Config) (bool, error) {
			err := cfg.ApplyDeploymentEmbeddings()
			return cfg.DeploymentEmbeddings != nil, err
		}},
		{name: "pg allow insecure", env: "AGENTSVIEW_PG_ALLOW_INSECURE", value: "true", apply: func(cfg *Config) (bool, error) {
			pg, err := cfg.ResolvePG()
			return pg.AllowInsecure, err
		}},
		{name: "hosted raw auth policy", env: "AGENTSVIEW_REQUIRE_AUTH", value: "false", validate: func(cfg *Config) error {
			// Applying false would fail with the auth requirement instead.
			return cfg.ValidatePGRawDerivation(PGConfig{Schema: "tenant_schema", RawTenant: "tenant"})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			writeConfig(t, dir, map[string]any{"pg": map[string]any{"url": "postgres://db.example/archive"}})
			cfg, err := LoadMinimalFor(ScopeLocal)
			require.NoError(t, err)
			cfg.RequireAuth = true
			t.Setenv("AGENTSVIEW_MODE", "srve")
			check := func() error {
				if tt.validate != nil {
					return tt.validate(&cfg)
				}
				applied, err := tt.apply(&cfg)
				assert.False(t, applied)
				return err
			}
			require.NoError(t, check(), "a reader with no variables set ignores the mode")

			value := tt.value
			if strings.HasSuffix(tt.env, "_FILE") {
				value = writeSecret(t, t.TempDir(), "secret", tt.value)
			}
			t.Setenv(tt.env, value)
			require.ErrorContains(t, check(), `AGENTSVIEW_MODE must be "serve" or "pg-serve", got "srve"`)
		})
	}
}

func TestScopedPGRawDeploymentPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, env, want string
		configured      bool
		hosted          bool
	}{
		{"enabled", "true", "", false, true},
		{"disabled overrides file", "false", "requires authentication", true, true},
		{"invalid policy", "invalid", "AGENTSVIEW_REQUIRE_AUTH must be a boolean", true, true},
		{"unused policy", "invalid", "", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			unsetDeploymentEnv(t)
			t.Setenv("AGENTSVIEW_MODE", "serve")
			t.Setenv("AGENTSVIEW_REQUIRE_AUTH", tt.env)
			t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", filepath.Join(t.TempDir(), "missing-token"))
			pg := PGConfig{Schema: "tenant_schema"}
			if tt.hosted {
				pg.RawTenant = "tenant"
			}
			cfg := Config{RequireAuth: tt.configured}
			err := cfg.ValidatePGRawDerivation(pg)
			if tt.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.want)
			}
		})
	}
}

func TestScopedTargetLayoutCannotHideParseErrors(t *testing.T) {
	for _, data := range []string{
		"[pg.all]\nurl=123",
		"[clickhouse.all]\nurl=123",
		"[pg.'']\nurl=123",
		"[pg.Primary]\nurl='postgres://db.example/archive'\n[pg.primary]\nurl=123",
		"[pg]\nurl=123\n[pg.primary]\nurl='postgres://db.example/archive'",
		"[pg]\nunknown_key=true\n[pg.all]\nurl='postgres://db.example/archive'",
	} {
		t.Run(data, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(data), 0o600))
			_, err := LoadMinimalFor(ScopeLocal)
			require.Error(t, err, "parse errors must remain global even in an invalid unused target")
		})
	}
}

func TestScopedReadOnlyDoesNotGenerateSecrets(t *testing.T) {
	unsetDeploymentEnv(t)
	dir := setupTestEnv(t)
	data := []byte("[insights]\nmodel='incomplete'\n")
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	_, err := LoadReadOnlyFor(ScopeLocal)
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "config.toml", entries[0].Name())
}

func TestScopedLoadersUnusedPresentation(t *testing.T) {
	for _, data := range []string{"chart_palette='invalid'", "zoom_level=12"} {
		t.Run(data, func(t *testing.T) {
			unsetDeploymentEnv(t)
			dir := setupTestEnv(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(data), 0o600))
			_, err := LoadReadOnlyFor(ScopeLocal)
			require.NoError(t, err)
			_, err = LoadReadOnly()
			require.Error(t, err)
		})
	}
}

// Any remote plaintext endpoint is invalid for insights without allow_http,
// regardless of its path. It must remain intact and usable by unrelated loads.
func FuzzScopedInsightsEndpoint(f *testing.F) {
	f.Add("", false)
	f.Add("v1/chat/completions", true)
	f.Add("quotes'\"\n雪", false)
	f.Fuzz(func(t *testing.T, path string, allowHTTP bool) {
		unsetDeploymentEnv(t)
		dir := setupTestEnv(t)
		endpoint := "http://models.example/" + url.PathEscape(path)
		writeConfig(t, dir, map[string]any{"insights": map[string]any{"endpoint": endpoint, "model": "remote", "allow_http": allowHTTP}})
		for _, load := range []func() (Config, error){
			func() (Config, error) { return LoadMinimalFor(ScopeLocal) },
			func() (Config, error) { return LoadReadOnlyFor(ScopeLocal) },
			func() (Config, error) {
				return LoadPFlagsFor(pflag.NewFlagSet("test", pflag.ContinueOnError), ScopeLocal)
			},
		} {
			cfg, err := load()
			require.NoError(t, err)
			assert.Equal(t, endpoint, cfg.Insights.Endpoint)
			assert.Equal(t, "remote", cfg.Insights.Model)
		}
		_, err := LoadMinimal()
		if allowHTTP {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, "plaintext")
		}
	})
}
