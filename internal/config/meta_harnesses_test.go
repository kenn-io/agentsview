package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestDefault_IncludesAntigravityCLIMetaHarnessContainer(t *testing.T) {
	home := canonicalTempDir(t)
	setTestHome(t, home)
	t.Setenv("T3CODE_HOME", "")

	cfg, err := Default()
	require.NoError(t, err)
	dirs := cfg.ResolveDirs(parser.AgentAntigravityCLI)

	assert.Equal(t, []string{
		filepath.Join(home, ".gemini", "antigravity-cli"),
		filepath.Join(home, ".t3", "userdata", "providers", "antigravity"),
	}, dirs)
}

func TestDefault_T3CodeHomeRedirectsAntigravityCLIContainer(t *testing.T) {
	home := canonicalTempDir(t)
	setTestHome(t, home)
	customT3 := filepath.Join(canonicalTempDir(t), "custom-t3")
	t.Setenv("T3CODE_HOME", customT3)

	cfg, err := Default()
	require.NoError(t, err)
	dirs := cfg.ResolveDirs(parser.AgentAntigravityCLI)

	assert.Equal(t, []string{
		filepath.Join(home, ".gemini", "antigravity-cli"),
		filepath.Join(customT3, "userdata", "providers", "antigravity"),
	}, dirs)
}

func TestDefault_RelativeT3CodeHomeFallsBackToHome(t *testing.T) {
	home := canonicalTempDir(t)
	setTestHome(t, home)
	t.Setenv("T3CODE_HOME", "relative/t3")

	cfg, err := Default()
	require.NoError(t, err)
	dirs := cfg.ResolveDirs(parser.AgentAntigravityCLI)

	assert.Contains(t, dirs, filepath.Join(home, ".t3", "userdata", "providers", "antigravity"))
}

func TestDefault_AntigravityCLIEnvReplacesMetaHarnessRoots(t *testing.T) {
	home := canonicalTempDir(t)
	setTestHome(t, home)
	t.Setenv("T3CODE_HOME", "")
	custom := filepath.Join(canonicalTempDir(t), "custom-antigravity-cli")
	t.Setenv("ANTIGRAVITY_CLI_DIR", custom)

	cfg, err := Default()
	require.NoError(t, err)
	cfg.loadEnv()

	assert.Equal(t, []string{custom}, cfg.ResolveDirs(parser.AgentAntigravityCLI))
}

func TestLoad_AntigravityCLIConfigDirsReplaceMetaHarnessRoots(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CLI_DIR", "")
	t.Setenv("T3CODE_HOME", "")
	custom := filepath.Join(canonicalTempDir(t), "custom-harness-antigravity")
	require.NoError(t, os.MkdirAll(custom, 0o755))

	cfg := loadMinimalWithConfig(t, map[string]any{
		"agents": map[string]any{
			"antigravity-cli": map[string]any{
				"dirs": []string{custom},
			},
		},
	})

	assert.Equal(t, []string{custom}, cfg.ResolveDirs(parser.AgentAntigravityCLI))
	assert.True(t, cfg.IsUserConfigured(parser.AgentAntigravityCLI))
}
