package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInsightsConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  InsightsConfig
		wantErr string
	}{
		{name: "absent"},
		{name: "loopback", config: InsightsConfig{Endpoint: "http://127.0.0.1:30000/v1", Model: "local"}},
		{name: "https", config: InsightsConfig{Endpoint: "https://models.example/v1", Model: "remote"}},
		{name: "remote http opt in", config: InsightsConfig{Endpoint: "http://models.example/v1", Model: "remote", AllowHTTP: true}},
		{name: "partial endpoint", config: InsightsConfig{Endpoint: "http://127.0.0.1:30000/v1"}, wantErr: "required together"},
		{name: "partial model", config: InsightsConfig{Model: "local"}, wantErr: "required together"},
		{name: "userinfo", config: InsightsConfig{Endpoint: "https://user:secret@models.example/v1", Model: "remote"}, wantErr: "credentials"},
		{name: "remote http", config: InsightsConfig{Endpoint: "http://models.example/v1", Model: "remote"}, wantErr: "plaintext"},
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
		})
	}
}

func TestInsightsConfigAPIKey(t *testing.T) {
	t.Setenv("AGENTSVIEW_TEST_INSIGHTS_KEY", "secret")
	c := InsightsConfig{APIKeyEnv: " AGENTSVIEW_TEST_INSIGHTS_KEY "}
	assert.Equal(t, "secret", c.APIKey())
	t.Setenv("AGENTSVIEW_TEST_INSIGHTS_KEY", "")
	assert.Empty(t, c.APIKey())
	assert.NotContains(t, os.Getenv("AGENTSVIEW_TEST_INSIGHTS_KEY"), "secret")
}

func TestInsightsConfigTOMLLoadAndFinalize(t *testing.T) {
	cfg := loadMinimalWithConfig(t, map[string]any{
		"insights": map[string]any{
			"endpoint":    " http://127.0.0.1:11434/v1 ",
			"model":       " llama3.1 ",
			"api_key_env": " AGENTSVIEW_INSIGHTS_KEY ",
			"allow_http":  true,
		},
	})
	assert.Equal(t, "http://127.0.0.1:11434/v1", cfg.Insights.Endpoint)
	assert.Equal(t, "llama3.1", cfg.Insights.Model)
	assert.Equal(t, "AGENTSVIEW_INSIGHTS_KEY", cfg.Insights.APIKeyEnv)
	assert.True(t, cfg.Insights.AllowHTTP)

	err := loadMinimalErrWithConfig(t, map[string]any{
		"insights": map[string]any{
			"endpoint": "http://models.example/v1",
			"model":    "remote",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext")
}
