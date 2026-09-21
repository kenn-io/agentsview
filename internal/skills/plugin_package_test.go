package skills

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func memoryPluginRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..",
		"plugins", "agentsview-memory"))
}

func TestMemoryPluginHookMissingBinaryDoesNotBlockSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh script behavior test")
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(
		memoryPluginRoot(t), "scripts", "session-start.sh"))
	cmd.Env = []string{"PATH="}
	output, err := cmd.CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(output), "agentsview is not installed")
}

func TestMemoryPluginGeneratedArtifactsAreCurrent(t *testing.T) {
	artifacts, err := RenderPluginPackage("0.1.0")
	require.NoError(t, err)
	for _, artifact := range artifacts {
		body, readErr := os.ReadFile(filepath.Join(
			memoryPluginRoot(t), artifact.RelativePath))
		require.NoError(t, readErr)
		assert.Equal(t, artifact.Content, string(body), artifact.RelativePath)
	}
}

func TestMemoryPluginManifestsSelectFocusedMCP(t *testing.T) {
	root := memoryPluginRoot(t)
	for _, relative := range []string{
		".claude-plugin/plugin.json", ".codex-plugin/plugin.json", ".mcp.json",
		"hooks/hooks.json",
	} {
		body, err := os.ReadFile(filepath.Join(root, relative))
		require.NoError(t, err)
		var parsed any
		require.NoError(t, json.Unmarshal(body, &parsed), relative)
	}

	mcp, err := os.ReadFile(filepath.Join(root, ".mcp.json"))
	require.NoError(t, err)
	assert.Contains(t, string(mcp), `"--profile", "memory"`)
	assert.NotContains(t, string(mcp), "AGENTSVIEW_SERVER_TOKEN\"")
}
