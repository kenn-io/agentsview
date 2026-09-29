package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKataConfigValidate(t *testing.T) {
	base := KataConfig{Enabled: true, Project: "agentsview", Actor: "agentsview", Timeout: 10 * time.Second}
	with := func(mut func(*KataConfig)) KataConfig {
		c := base
		mut(&c)
		return c
	}
	tests := []struct {
		name    string
		config  KataConfig
		wantErr string
		noLeak  string
	}{
		{name: "disabled_ignores_everything", config: KataConfig{Endpoint: "ftp://x"}},
		{name: "disabled_ignores_query", config: KataConfig{Endpoint: "unix:///run/kata/daemon.sock?token=secret-value"}},
		{name: "discovery", config: base},
		{name: "unix_socket", config: with(func(c *KataConfig) { c.Endpoint = "unix:///run/kata/daemon.sock" })},
		{name: "https_with_token_env", config: with(func(c *KataConfig) { c.Endpoint = "https://kata.example.test"; c.TokenEnv = "KATA_TOKEN" })},
		{name: "https_without_token_env", config: with(func(c *KataConfig) { c.Endpoint = "https://kata.example.test" }), wantErr: "[kata] token_env is required for https endpoints"},
		{name: "loopback_http", config: with(func(c *KataConfig) { c.Endpoint = "http://127.0.0.1:7777" })},
		{name: "localhost_http", config: with(func(c *KataConfig) { c.Endpoint = "http://localhost:7777" })},
		{name: "remote_http_refused", config: with(func(c *KataConfig) { c.Endpoint = "http://kata.example.test" }), wantErr: "[kata] endpoint \"http://kata.example.test\" uses plaintext http"},
		{name: "remote_http_allow_insecure", config: with(func(c *KataConfig) { c.Endpoint = "http://kata.example.test"; c.AllowInsecure = true })},
		{name: "userinfo_refused", config: with(func(c *KataConfig) { c.Endpoint = "https://u:p@kata.example.test"; c.TokenEnv = "KATA_TOKEN" }), wantErr: "[kata] endpoint must not contain URL credentials"},
		{name: "unix_query_refused", config: with(func(c *KataConfig) { c.Endpoint = "unix:///run/kata/daemon.sock?token=secret-value" }), wantErr: "[kata] endpoint must not contain a query or fragment", noLeak: "secret-value"},
		{name: "https_query_refused", config: with(func(c *KataConfig) {
			c.Endpoint = "https://kata.example.test?token=secret-value"
			c.TokenEnv = "KATA_TOKEN"
		}), wantErr: "[kata] endpoint must not contain a query or fragment", noLeak: "secret-value"},
		{name: "http_query_refused", config: with(func(c *KataConfig) { c.Endpoint = "http://127.0.0.1:7777?token=secret-value" }), wantErr: "[kata] endpoint must not contain a query or fragment", noLeak: "secret-value"},
		{name: "empty_query_refused", config: with(func(c *KataConfig) { c.Endpoint = "https://kata.example.test?"; c.TokenEnv = "KATA_TOKEN" }), wantErr: "[kata] endpoint must not contain a query or fragment", noLeak: "?"},
		{name: "fragment_refused", config: with(func(c *KataConfig) { c.Endpoint = "https://kata.example.test#secret-value"; c.TokenEnv = "KATA_TOKEN" }), wantErr: "[kata] endpoint must not contain a query or fragment", noLeak: "secret-value"},
		{name: "relative_unix_refused", config: with(func(c *KataConfig) { c.Endpoint = "unix://relative.sock" }), wantErr: "[kata] endpoint must be unix:///absolute/path"},
		{name: "relative_unix_error_redacts_query", config: with(func(c *KataConfig) { c.Endpoint = "unix://relative.sock?token=secret-value" }), wantErr: "[kata] endpoint must not contain a query or fragment", noLeak: "secret-value"},
		{name: "relative_unix_error_redacts_path", config: with(func(c *KataConfig) { c.Endpoint = "unix://relative.sock/secret-value" }), wantErr: "[kata] endpoint must be unix:///absolute/path", noLeak: "secret-value"},
		{name: "bad_scheme", config: with(func(c *KataConfig) { c.Endpoint = "ftp://kata.example.test" }), wantErr: "must be unix://, https:// or http://"},
		{name: "empty_project", config: with(func(c *KataConfig) { c.Project = "  " }), wantErr: "[kata] project must be non-empty"},
		{name: "zero_timeout", config: with(func(c *KataConfig) { c.Timeout = 0 }), wantErr: "[kata] timeout must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			if tt.noLeak != "" {
				assert.NotContains(t, err.Error(), tt.noLeak)
			}
		})
	}
}

func TestKataConfigToken(t *testing.T) {
	t.Setenv("AGENTSVIEW_TEST_KATA_TOKEN", "tok-value")
	assert.Equal(t, "tok-value", KataConfig{TokenEnv: " AGENTSVIEW_TEST_KATA_TOKEN "}.Token())
	assert.Empty(t, KataConfig{}.Token())
	t.Setenv("AGENTSVIEW_TEST_KATA_TOKEN", "")
	assert.Empty(t, KataConfig{TokenEnv: "AGENTSVIEW_TEST_KATA_TOKEN"}.Token())
}

func TestKataConfigTOMLLoadAndDefaults(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want KataConfig
	}{
		{name: "absent_section_keeps_defaults", data: map[string]any{}, want: KataConfig{Project: "agentsview", Actor: "agentsview", Timeout: 10 * time.Second}},
		{name: "partial_section_keeps_unset_defaults", data: map[string]any{"kata": map[string]any{"enabled": true, "endpoint": " unix:///tmp/k.sock "}}, want: KataConfig{Enabled: true, Endpoint: "unix:///tmp/k.sock", Project: "agentsview", Actor: "agentsview", Timeout: 10 * time.Second}},
		{name: "full_section", data: map[string]any{"kata": map[string]any{"enabled": true, "endpoint": "https://kata.example.test", "token_env": " KATA_TOKEN ", "project": "friction", "actor": "av-bot", "allow_insecure": false, "timeout": "3s"}}, want: KataConfig{Enabled: true, Endpoint: "https://kata.example.test", TokenEnv: "KATA_TOKEN", Project: "friction", Actor: "av-bot", Timeout: 3 * time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadMinimalWithConfig(t, tt.data)
			assert.Equal(t, tt.want, cfg.Kata)
		})
	}
}

func TestKataConfigTOMLFinalizeRejectsHTTPSWithoutToken(t *testing.T) {
	err := loadMinimalErrWithConfig(t, map[string]any{"kata": map[string]any{"enabled": true, "endpoint": "https://kata.example.test"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[kata] token_env is required for https endpoints")
}

func TestKataEnabledOnPusherIsNotAConfigError(t *testing.T) {
	cfg := loadMinimalWithConfig(t, map[string]any{"pg": map[string]any{"url": "postgres://pg.example.test/agentsview"}, "kata": map[string]any{"enabled": true, "endpoint": "unix:///tmp/k.sock"}})
	assert.True(t, cfg.Kata.Enabled)
	assert.True(t, cfg.HasPGPushTarget())
}

func TestHasPGPushTarget(t *testing.T) {
	t.Setenv("AGENTSVIEW_PG_URL", "")
	tests := []struct {
		name string
		data map[string]any
		want bool
	}{
		{name: "no_pg", data: map[string]any{}, want: false},
		{name: "pg_section_without_url", data: map[string]any{"pg": map[string]any{"schema": "x"}}, want: false},
		{name: "legacy_url", data: map[string]any{"pg": map[string]any{"url": "postgres://pg.example.test/a"}}, want: true},
		{name: "named_url", data: map[string]any{"pg": map[string]any{"work": map[string]any{"url": "postgres://pg.example.test/a"}}}, want: true},
		{name: "unresolvable_named_target", data: map[string]any{"default_pg": "missing", "pg": map[string]any{"work": map[string]any{"url": "postgres://pg.example.test/a"}}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadMinimalWithConfig(t, tt.data)
			assert.Equal(t, tt.want, cfg.HasPGPushTarget())
		})
	}
}
