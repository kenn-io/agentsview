package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageTextConfigPresence(t *testing.T) {
	for _, tc := range []struct {
		name, content       string
		configured, enabled bool
	}{
		{name: "absent"},
		{name: "enabled", content: "[image_text]\nenabled = true\nendpoint = 'http://127.0.0.1:1234/glmocr/parse'\n", configured: true, enabled: true},
		{name: "disabled", content: "[image_text]\nenabled = false\n", configured: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tc.content), 0o600))
			cfg, err := LoadReadOnlyAt(dir)
			require.NoError(t, err)
			require.Equal(t, tc.configured, cfg.ImageTextConfigured)
			require.Equal(t, tc.enabled, cfg.ImageText.Enabled)
		})
	}
}

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
