package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func readConfigFile(t *testing.T, dir string) Config {
	t.Helper()
	var fileCfg Config
	_, err := toml.DecodeFile(
		filepath.Join(dir, configFileName), &fileCfg,
	)
	require.NoError(t, err, "parsing config file")
	return fileCfg
}

func TestSaveSettingsPersistsChartPalette(t *testing.T) {
	dir := setupTestEnv(t)
	cfg, err := Default()
	require.NoError(t, err)
	cfg.DataDir = dir
	require.NoError(t, cfg.SaveSettings(map[string]any{
		"chart_palette": ChartPaletteMatplotlib,
	}))
	assert.Equal(t, ChartPaletteMatplotlib, cfg.ChartPalette)
	fileCfg := readConfigFile(t, dir)
	assert.Equal(t, ChartPaletteMatplotlib, fileCfg.ChartPalette)
}

func TestSaveSettingsRejectsInvalidChartPaletteWithoutChangingSelection(t *testing.T) {
	dir := setupTestEnv(t)
	cfg, err := Default()
	require.NoError(t, err)
	cfg.DataDir = dir
	require.NoError(t, cfg.SaveSettings(map[string]any{
		"chart_palette": ChartPaletteMatplotlib,
	}))

	err = cfg.SaveSettings(map[string]any{
		"chart_palette": ChartPalette("neon"),
	})
	require.EqualError(t, err,
		`chart_palette must be "agentsview" or "matplotlib" (got "neon")`)
	assert.Equal(t, ChartPaletteMatplotlib, cfg.ChartPalette)
	fileCfg := readConfigFile(t, dir)
	assert.Equal(t, ChartPaletteMatplotlib, fileCfg.ChartPalette)
}

func TestSaveSettingsPersistsDisabledAgents(t *testing.T) {
	dir := setupTestEnv(t)
	cfg, err := Default()
	require.NoError(t, err)
	cfg.DataDir = dir

	require.NoError(t, cfg.SaveSettings(map[string]any{
		"disabled_agents": []parser.AgentType{
			parser.AgentGemini,
			parser.AgentClaude,
			parser.AgentGemini,
		},
	}))

	assert.Equal(t,
		[]parser.AgentType{parser.AgentClaude, parser.AgentGemini},
		cfg.DisabledAgents,
	)
	fileCfg := readConfigFile(t, dir)
	assert.Equal(t, cfg.DisabledAgents, fileCfg.DisabledAgents)
}

func TestCursorSecret_GeneratedAndPersisted(t *testing.T) {
	dir := setupTestEnv(t)

	// First load: should generate a secret
	cfg1, err := LoadMinimal()
	require.NoError(t, err, "first load failed")
	require.NotEmpty(t, cfg1.CursorSecret, "cursor secret was not generated")
	require.Equal(t, dir, cfg1.DataDir)

	// Verify file existence and content
	fileCfg := readConfigFile(t, dir)

	assert.Equal(t, cfg1.CursorSecret, fileCfg.CursorSecret)

	// Second load: should read the same secret
	cfg2, err := LoadMinimal()
	require.NoError(t, err, "second load failed")
	assert.Equal(t, cfg1.CursorSecret, cfg2.CursorSecret)
}

func TestCursorSecret_RegeneratedIfMissing(t *testing.T) {
	dir := setupTestEnv(t)

	initialContent := "cursor_secret = \"\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, configFileName), []byte(initialContent), 0o600))

	cfg, err := LoadMinimal()
	require.NoError(t, err)
	require.NotEmpty(t, cfg.CursorSecret, "cursor secret should have been regenerated")

	// Verify it was updated in the file
	fileCfg := readConfigFile(t, dir)
	assert.NotEmpty(t, fileCfg.CursorSecret, "cursor secret was not updated in the file")
}

func TestCursorSecret_LoadErrorOnInvalidConfig(t *testing.T) {
	dir := setupTestEnv(t)

	require.NoError(t, os.WriteFile(filepath.Join(dir, configFileName), []byte("[invalid toml = ="), 0o600))

	_, err := LoadMinimal()
	require.Error(t, err, "expected error loading invalid config")
}

func TestCursorSecret_PreservesOtherFields(t *testing.T) {
	dir := setupTestEnv(t)

	require.NoError(t, os.WriteFile(filepath.Join(dir, configFileName), []byte("github_token = \"my-token\"\n"), 0o600))

	cfg, err := LoadMinimal()
	require.NoError(t, err)

	assert.NotEmpty(t, cfg.CursorSecret, "cursor secret not generated")
	assert.Equal(t, "my-token", cfg.GithubToken)

	// Verify file content has both
	fileCfg := readConfigFile(t, dir)

	assert.NotEmpty(t, fileCfg.CursorSecret, "cursor_secret missing in file")
	assert.Equal(t, "my-token", fileCfg.GithubToken, "github_token lost/changed in file")
}
