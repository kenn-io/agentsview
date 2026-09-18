package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCaptureReturnsOnlyExactCustomPricing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	configTOML := `
auth_token = "must-not-be-returned"
host = "0.0.0.0"

[custom_model_pricing."model-test"]
input_microdollars_per_mtok = 123
output_microdollars_per_mtok = 456
cache_creation_microdollars_per_mtok = 789
cache_read_microdollars_per_mtok = 12
`
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "config.toml"), []byte(configTOML), 0o600))

	cfg, err := LoadCapture()
	require.NoError(t, err)
	assert.Equal(t, map[string]CustomModelRate{
		"model-test": {
			InputMicrodollarsPerMTok: 123, OutputMicrodollarsPerMTok: 456,
			CacheCreationMicrodollarsPerMTok: 789, CacheReadMicrodollarsPerMTok: 12,
		},
	}, cfg.CustomModelPricing)
}

func TestLoadCaptureUsesExplicitDataDirWithoutHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")

	cfg, err := LoadCapture()
	require.NoError(t, err)
	assert.Empty(t, cfg.CustomModelPricing)
}

func TestLoadCaptureReadsLegacyJSONWithoutMigratingIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "auth_token": "must-not-be-returned",
  "custom_model_pricing": {
    "model-test": {
      "input_microdollars_per_mtok": 123,
      "output_microdollars_per_mtok": 456,
      "cache_creation_microdollars_per_mtok": 789,
      "cache_read_microdollars_per_mtok": 12
    }
  }
}`), 0o600))

	cfg, err := LoadCapture()
	require.NoError(t, err)
	assert.Equal(t, int64(123),
		cfg.CustomModelPricing["model-test"].InputMicrodollarsPerMTok)
	assert.FileExists(t, path)
	assert.NoFileExists(t, filepath.Join(dir, "config.toml"))
}
