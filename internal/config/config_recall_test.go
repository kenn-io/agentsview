package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validRecallExtractConfig returns a RecallExtractConfig that passes
// Validate, so each case below can mutate exactly the field under test.
func validRecallExtractConfig() RecallExtractConfig {
	return RecallExtractConfig{
		Enabled:          true,
		Model:            "qwen3.5-27b",
		Server:           "local",
		MaxWindowChars:   50000,
		QuietPeriod:      "30m",
		BackstopInterval: "1h",
		FailureBackoff:   "1h",
		Servers: map[string]RecallExtractServerConfig{
			"local": {
				Endpoint: "http://127.0.0.1:30000/v1",
				Timeout:  "120s",
			},
			"remote": {
				Endpoint:  "http://build-box:30000/v1",
				Timeout:   "300s",
				AllowHTTP: true,
			},
		},
	}
}

func TestRecallExtractConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*RecallExtractConfig)
		wantErr string
	}{
		{
			name:   "disabled is valid even with empty fields",
			mutate: func(c *RecallExtractConfig) { *c = RecallExtractConfig{} },
		},
		{
			name:    "enabled missing model",
			mutate:  func(c *RecallExtractConfig) { c.Model = "" },
			wantErr: "model is required",
		},
		{
			name:    "enabled with no servers",
			mutate:  func(c *RecallExtractConfig) { c.Servers = nil },
			wantErr: "at least one server",
		},
		{
			name:    "multiple servers without server selection",
			mutate:  func(c *RecallExtractConfig) { c.Server = "" },
			wantErr: "server is required",
		},
		{
			name:    "server names an undefined server",
			mutate:  func(c *RecallExtractConfig) { c.Server = "nope" },
			wantErr: `server "nope" is not a defined server`,
		},
		{
			name: "single server needs no selection",
			mutate: func(c *RecallExtractConfig) {
				delete(c.Servers, "remote")
				c.Server = ""
			},
		},
		{
			name: "server missing endpoint",
			mutate: func(c *RecallExtractConfig) {
				s := c.Servers["local"]
				s.Endpoint = ""
				c.Servers["local"] = s
			},
			wantErr: "[recall.extract.servers.local] endpoint is required",
		},
		{
			name: "server endpoint not http",
			mutate: func(c *RecallExtractConfig) {
				s := c.Servers["local"]
				s.Endpoint = "ftp://example/v1"
				c.Servers["local"] = s
			},
			wantErr: "endpoint",
		},
		{
			name: "server bad timeout",
			mutate: func(c *RecallExtractConfig) {
				s := c.Servers["local"]
				s.Timeout = "soon"
				c.Servers["local"] = s
			},
			wantErr: "timeout",
		},
		{
			name:    "zero max_window_chars",
			mutate:  func(c *RecallExtractConfig) { c.MaxWindowChars = 0 },
			wantErr: "max_window_chars",
		},
		{
			name:    "bad quiet_period",
			mutate:  func(c *RecallExtractConfig) { c.QuietPeriod = "later" },
			wantErr: "quiet_period",
		},
		{
			name:    "negative quiet_period",
			mutate:  func(c *RecallExtractConfig) { c.QuietPeriod = "-5m" },
			wantErr: "quiet_period",
		},
		{
			name:    "bad backstop_interval",
			mutate:  func(c *RecallExtractConfig) { c.BackstopInterval = "x" },
			wantErr: "backstop_interval",
		},
		{
			name: "zero backstop_interval",
			mutate: func(c *RecallExtractConfig) {
				c.BackstopInterval = "0s"
			},
			wantErr: "backstop_interval must not be zero",
		},
		{
			name: "negative backstop_interval disables and is valid",
			mutate: func(c *RecallExtractConfig) {
				c.BackstopInterval = "-1s"
			},
		},
		{
			name:    "bad failure_backoff",
			mutate:  func(c *RecallExtractConfig) { c.FailureBackoff = "x" },
			wantErr: "failure_backoff",
		},
		{
			name:    "negative max_tokens",
			mutate:  func(c *RecallExtractConfig) { c.MaxTokens = -1 },
			wantErr: "max_tokens",
		},
		{
			name: "plaintext http to a non-loopback host",
			mutate: func(c *RecallExtractConfig) {
				s := c.Servers["remote"]
				s.AllowHTTP = false
				c.Servers["remote"] = s
			},
			wantErr: "allow_http",
		},
		{
			name: "https to a non-loopback host",
			mutate: func(c *RecallExtractConfig) {
				s := c.Servers["remote"]
				s.Endpoint = "https://build-box:30000/v1"
				s.AllowHTTP = false
				c.Servers["remote"] = s
			},
		},
		{
			name: "plaintext http to localhost by name",
			mutate: func(c *RecallExtractConfig) {
				s := c.Servers["local"]
				s.Endpoint = "http://localhost:30000/v1"
				c.Servers["local"] = s
			},
		},
		{
			name: "plaintext http to IPv6 loopback",
			mutate: func(c *RecallExtractConfig) {
				s := c.Servers["local"]
				s.Endpoint = "http://[::1]:30000/v1"
				c.Servers["local"] = s
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validRecallExtractConfig()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestRecallExtractServerResolution(t *testing.T) {
	cfg := validRecallExtractConfig()
	name, server, err := cfg.ResolvedServer()
	require.NoError(t, err)
	assert.Equal(t, "local", name)
	assert.Equal(t, "http://127.0.0.1:30000/v1", server.Endpoint)

	cfg.Server = "remote"
	name, server, err = cfg.ResolvedServer()
	require.NoError(t, err)
	assert.Equal(t, "remote", name)
	assert.Equal(t, "http://build-box:30000/v1", server.Endpoint)

	cfg.Server = ""
	_, _, err = cfg.ResolvedServer()
	require.Error(t, err, "ambiguous selection with two servers")

	cfg.Servers = map[string]RecallExtractServerConfig{
		"only": {Endpoint: "http://one/v1", Timeout: "120s"},
	}
	name, _, err = cfg.ResolvedServer()
	require.NoError(t, err)
	assert.Equal(t, "only", name, "a single server resolves without selection")
}

func TestRecallExtractConfigDefaults(t *testing.T) {
	cfg, err := Default()
	require.NoError(t, err)

	extract := cfg.Recall.Extract
	assert.False(t, extract.Enabled)
	assert.Equal(t, 50000, extract.MaxWindowChars)
	assert.Equal(t, "30m", extract.QuietPeriod)
	assert.Equal(t, "1h", extract.BackstopInterval)
	assert.Equal(t, "1h", extract.FailureBackoff)
	assert.Zero(t, extract.MaxTokens,
		"unset max_tokens defers to the prompt profile default")
	assert.Empty(t, extract.Servers)
}

func TestRecallExtractConfigTOMLLoad(t *testing.T) {
	cfg := loadMinimalWithConfig(t, map[string]any{
		"recall": map[string]any{
			"extract": map[string]any{
				"enabled":    true,
				"model":      "qwen3.5-27b",
				"deployment": "gpu-a",
				"server":     "local",
				"max_tokens": 8192,
				"servers": map[string]any{
					"local": map[string]any{
						"endpoint": "http://127.0.0.1:30000/v1",
					},
					"slow": map[string]any{
						"endpoint":   "http://build-box:30000/v1",
						"timeout":    "600s",
						"allow_http": true,
					},
				},
				"prompts": map[string]any{
					"profile": "qwen",
					"dir":     "/etc/agentsview/prompts",
				},
				"request": map[string]any{
					"temperature": 0.2,
					"extra_body": map[string]any{
						"chat_template_kwargs": map[string]any{
							"enable_thinking": false,
						},
					},
				},
			},
		},
	})

	extract := cfg.Recall.Extract
	require.True(t, extract.Enabled)
	assert.Equal(t, "qwen3.5-27b", extract.Model)
	assert.Equal(t, "gpu-a", extract.Deployment)
	assert.Equal(t, "local", extract.Server)
	assert.Equal(t, 8192, extract.MaxTokens)
	assert.Equal(t, 50000, extract.MaxWindowChars, "unset keeps default")
	assert.Equal(t, "30m", extract.QuietPeriod, "unset keeps default")
	assert.Equal(t, "1h", extract.BackstopInterval, "unset keeps default")
	assert.Equal(t, "http://127.0.0.1:30000/v1",
		extract.Servers["local"].Endpoint)
	assert.Equal(t, "120s", extract.Servers["local"].Timeout,
		"unset timeout keeps default")
	assert.Equal(t, "600s", extract.Servers["slow"].Timeout)
	assert.True(t, extract.Servers["slow"].AllowHTTP,
		"allow_http opts a non-loopback plaintext endpoint in")
	assert.Equal(t, "qwen", extract.Prompts.Profile)
	assert.Equal(t, "/etc/agentsview/prompts", extract.Prompts.Dir)
	require.NotNil(t, extract.Request.Temperature)
	assert.Equal(t, 0.2, *extract.Request.Temperature)
	kwargs, ok := extract.Request.ExtraBody["chat_template_kwargs"].(map[string]any)
	require.True(t, ok, "extra_body nested tables decode as maps")
	assert.Equal(t, false, kwargs["enable_thinking"])
}

func TestRecallExtractConfigTOMLLoadInvalid(t *testing.T) {
	err := loadMinimalErrWithConfig(t, map[string]any{
		"recall": map[string]any{
			"extract": map[string]any{
				"enabled": true,
				"model":   "m",
			},
		},
	})
	require.Error(t, err, "enabled without servers must fail at load")
}
