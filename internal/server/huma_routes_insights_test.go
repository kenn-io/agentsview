package server

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

func TestInsightGenerateOptionsMapsConfig(t *testing.T) {
	require := require.New(t)

	t.Setenv("AGENTSVIEW_INSIGHTS_KEY", "key")
	cfg := config.Config{
		Insights: config.InsightsConfig{
			Endpoint:  "http://127.0.0.1:30000/v1",
			Model:     "local-model",
			APIKeyEnv: "AGENTSVIEW_INSIGHTS_KEY",
			AllowHTTP: true,
		},
		Agent: map[string]config.AgentConfig{
			"gemini": {Binary: "gemini-bin", Sandbox: "sandbox", AllowUnsafe: true},
		},
	}
	opts := insightGenerateOptions(cfg)
	require.NotNil(opts.Endpoint)
	require.Equal("http://127.0.0.1:30000/v1", opts.Endpoint.Endpoint)
	require.Equal("local-model", opts.Endpoint.Model)
	require.Equal("key", opts.Endpoint.APIKey)
	require.True(opts.Endpoint.AllowHTTP)
	require.Equal("gemini-bin", opts.Agents["gemini"].Binary)
	require.Equal("sandbox", opts.Agents["gemini"].Sandbox)
	require.True(opts.Agents["gemini"].AllowUnsafe)
}
