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
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)
	cfg, err := Default()
	require.NoError(err)
	cfg.DataDir = dir
	require.NoError(cfg.SaveSettings(map[string]any{
		"chart_palette": ChartPaletteMatplotlib,
	}))
	assert.Equal(ChartPaletteMatplotlib, cfg.ChartPalette)
	fileCfg := readConfigFile(t, dir)
	assert.Equal(ChartPaletteMatplotlib, fileCfg.ChartPalette)
}

func TestSaveSettingsPersistsZoomLevelAndPreservesUnrelatedTables(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)
	require.NoError(os.WriteFile(filepath.Join(dir, configFileName), []byte(
		"github_token = \"keep\"\n[proxy]\nmode = \"caddy\"\n"), 0o600))
	cfg, err := Default()
	require.NoError(err)
	cfg.DataDir = dir

	require.NoError(cfg.SaveSettings(map[string]any{
		"zoom_level": ZoomLevel120,
	}))
	require.NotNil(cfg.ZoomLevel)
	assert.Equal(ZoomLevel120, *cfg.ZoomLevel)

	fileCfg := readConfigFile(t, dir)
	require.NotNil(fileCfg.ZoomLevel)
	assert.Equal(ZoomLevel120, *fileCfg.ZoomLevel)
	assert.Equal("keep", fileCfg.GithubToken)
	assert.Equal("caddy", fileCfg.Proxy.Mode)
}

func TestSaveSettingsRejectsInvalidZoomWithoutChangingSelectionOrDisk(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)
	cfg, err := Default()
	require.NoError(err)
	cfg.DataDir = dir
	require.NoError(cfg.SaveSettings(map[string]any{
		"zoom_level": ZoomLevel120,
	}))
	before, err := os.ReadFile(filepath.Join(dir, configFileName))
	require.NoError(err)

	err = cfg.SaveSettings(map[string]any{
		"zoom_level": ZoomLevel(101),
	})
	require.EqualError(err,
		"zoom_level must be one of 67, 75, 80, 90, 100, 110, 120, 125, 130, 150, 175, 200 (got 101)")
	require.NotNil(cfg.ZoomLevel)
	assert.Equal(ZoomLevel120, *cfg.ZoomLevel)
	after, err := os.ReadFile(filepath.Join(dir, configFileName))
	require.NoError(err)
	assert.Equal(before, after)

	err = cfg.SaveSettings(map[string]any{"zoom_level": 120})
	assert.EqualError(err, "zoom_level must use the typed configuration value")
}

func TestSaveSettingsRejectsInvalidChartPaletteWithoutChangingSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)
	cfg, err := Default()
	require.NoError(err)
	cfg.DataDir = dir
	require.NoError(cfg.SaveSettings(map[string]any{
		"chart_palette": ChartPaletteMatplotlib,
	}))

	err = cfg.SaveSettings(map[string]any{
		"chart_palette": ChartPalette("neon"),
	})
	require.EqualError(err,
		`chart_palette must be "agentsview" or "matplotlib" (got "neon")`)
	assert.Equal(ChartPaletteMatplotlib, cfg.ChartPalette)
	fileCfg := readConfigFile(t, dir)
	assert.Equal(ChartPaletteMatplotlib, fileCfg.ChartPalette)
}

func TestSaveSettingsPersistsDisabledAgents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)
	cfg, err := Default()
	require.NoError(err)
	cfg.DataDir = dir

	require.NoError(cfg.SaveSettings(map[string]any{
		"disabled_agents": []parser.AgentType{
			parser.AgentGemini,
			parser.AgentClaude,
			parser.AgentGemini,
		},
	}))

	assert.Equal([]parser.AgentType{parser.AgentClaude, parser.AgentGemini},
		cfg.DisabledAgents,
	)
	fileCfg := readConfigFile(t, dir)
	assert.Equal(cfg.DisabledAgents, fileCfg.DisabledAgents)
}

func TestCursorSecret_GeneratedAndPersisted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)

	// First load: should generate a secret
	cfg1, err := LoadMinimal()
	require.NoError(err, "first load failed")
	require.NotEmpty(cfg1.CursorSecret, "cursor secret was not generated")
	require.Equal(dir, cfg1.DataDir)

	// Verify file existence and content
	fileCfg := readConfigFile(t, dir)

	assert.Equal(cfg1.CursorSecret, fileCfg.CursorSecret)

	// Second load: should read the same secret
	cfg2, err := LoadMinimal()
	require.NoError(err, "second load failed")
	assert.Equal(cfg1.CursorSecret, cfg2.CursorSecret)
}

func TestCursorSecret_RegeneratedIfMissing(t *testing.T) {
	require := require.New(t)

	dir := setupTestEnv(t)

	initialContent := "cursor_secret = \"\"\n"
	require.NoError(os.WriteFile(filepath.Join(dir, configFileName), []byte(initialContent), 0o600))

	cfg, err := LoadMinimal()
	require.NoError(err)
	require.NotEmpty(cfg.CursorSecret, "cursor secret should have been regenerated")

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
	assert := assert.New(t)
	require := require.New(t)

	dir := setupTestEnv(t)

	require.NoError(os.WriteFile(filepath.Join(dir, configFileName), []byte("github_token = \"my-token\"\n"), 0o600))

	cfg, err := LoadMinimal()
	require.NoError(err)

	assert.NotEmpty(cfg.CursorSecret, "cursor secret not generated")
	assert.Equal("my-token", cfg.GithubToken)

	// Verify file content has both
	fileCfg := readConfigFile(t, dir)

	assert.NotEmpty(fileCfg.CursorSecret, "cursor_secret missing in file")
	assert.Equal("my-token", fileCfg.GithubToken, "github_token lost/changed in file")
}
