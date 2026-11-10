package config

import (
	"encoding/json/v2"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestChatGPTConfigModelProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		model := hegel.Draw(ht, hegel.Text())
		data, err := json.Marshal(map[string]string{"provider": "chatgpt", "model": model})
		require.NoError(ht, err)
		var cfg InsightsConfig
		require.NoError(ht, json.Unmarshal(data, &cfg))
		err = cfg.Validate()
		if strings.TrimSpace(model) == "" {
			require.Error(ht, err)
		} else {
			require.NoError(ht, err)
		}
	})
}

func TestChatGPTConfigConflicts(t *testing.T) {
	for _, extra := range []map[string]any{{"endpoint": "https://example.invalid/v1"}, {"api_key_env": "EXAMPLE_KEY"}, {"allow_http": true}} {
		raw := map[string]any{"provider": "chatgpt", "model": "model"}
		maps.Copy(raw, extra)
		data, err := json.Marshal(raw)
		require.NoError(t, err)
		var cfg InsightsConfig
		require.NoError(t, json.Unmarshal(data, &cfg))
		require.Error(t, cfg.Validate())
	}
	var unknown InsightsConfig
	require.NoError(t, json.Unmarshal([]byte(`{"provider":"unknown"}`), &unknown))
	require.ErrorContains(t, unknown.Validate(), "provider must be empty or chatgpt")
	cfg := loadMinimalWithConfig(t, map[string]any{"insights": map[string]any{"provider": " ChatGPT ", "model": " model "}})
	assert.Equal(t, "chatgpt", cfg.Insights.Provider)
	assert.Equal(t, "model", cfg.Insights.Model)
}

// The shrunk counterexample from the provider/model property was an empty model.
func TestChatGPTRejectsEmptyModel(t *testing.T) {
	var cfg InsightsConfig
	require.NoError(t, json.Unmarshal([]byte(`{"provider":"chatgpt","model":""}`), &cfg))
	require.ErrorContains(t, cfg.Validate(), "model")
}
