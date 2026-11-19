package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageTextConfig(t *testing.T) {
	cfg, err := Default()
	require.NoError(t, err)
	require.NoError(t, cfg.applyConfigTOML("[image_text]\nenabled = true\nendpoint = 'http://127.0.0.1:1234/glmocr/parse'\n"))
	require.True(t, cfg.ImageText.Enabled)
	policy, err := cfg.ImageText.Policy()
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:1234/glmocr/parse", policy.Values().Endpoint)
	require.NoError(t, cfg.validateSections())
	require.NoError(t, cfg.applyConfigTOML("[image_text]\nenabled = true\nendpoint = 'https://example.com/glmocr/parse'\n"))
	require.ErrorContains(t, cfg.validateSections(), "endpoint must be an explicit loopback HTTP")
}
