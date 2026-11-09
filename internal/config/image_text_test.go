package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/glmocr"
)

func TestImageTextConfig(t *testing.T) {
	var cfg Config
	require.NoError(t, cfg.applyConfigTOML("[image_text]\nenabled = true\nendpoint = 'http://127.0.0.1:1234/glmocr/parse'\n"))
	require.True(t, cfg.ImageText.Enabled)
	policy, err := cfg.ImageText.Policy()
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:1234/glmocr/parse", policy.Values().Endpoint)
	policy, err = (ImageTextConfig{}).Policy()
	require.NoError(t, err)
	require.Equal(t, glmocr.DefaultEndpoint, policy.Values().Endpoint)
	require.NoError(t, cfg.applyConfigTOML("[image_text]\nenabled = true\nendpoint = 'https://example.com/glmocr/parse'\n"))
	require.Error(t, cfg.ImageText.Validate())
}
