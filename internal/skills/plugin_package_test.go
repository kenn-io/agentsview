package skills

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

func memoryPluginRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..",
		"plugins", "agentsview-memory"))
	require.NoError(t, err)
	return root
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

func TestMemoryPluginAgentToolsMatchPluginServer(t *testing.T) {
	root := memoryPluginRoot(t)
	manifest, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
	require.NoError(t, err)
	var plugin struct {
		Name       string                     `json:"name"`
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(manifest, &plugin))
	require.NotEmpty(t, plugin.Name)
	require.Contains(t, plugin.MCPServers, "agentsview")
	prefix := "mcp__plugin_" + plugin.Name + "_agentsview__"
	agent, err := os.ReadFile(filepath.Join(root, "agents", "agentsview-search-conversations.md"))
	require.NoError(t, err)
	content := string(agent)
	frontmatter := strings.SplitN(content, "---\n", 3)
	require.Len(t, frontmatter, 3)
	var metadata struct {
		Tools []string `yaml:"tools"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(frontmatter[1]), &metadata))
	assert.Equal(t, []string{prefix + "search_content", prefix + "get_messages", "mcp__agentsview__search_content", "mcp__agentsview__get_messages"}, metadata.Tools)
	assert.Contains(t, content, "`"+prefix+"search_content`")
	assert.Contains(t, content, "`"+prefix+"get_messages`")
	assert.Contains(t, content, "`mcp__agentsview__search_content`")
	assert.Contains(t, content, "`mcp__agentsview__get_messages`")
}
