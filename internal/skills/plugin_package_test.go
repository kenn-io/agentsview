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
	// Outside any repository the hook inherits the user's environment; with
	// an empty PATH it reports the missing install and exits without
	// blocking the session.
	outside := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(
		memoryPluginRoot(t), "scripts", "session-start.sh"))
	cmd.Env = []string{"PATH=", "HOME=" + outside}
	cmd.Dir = outside
	output, err := cmd.CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(output), "agentsview is not installed")
}

// Inside a repository the hook must fail closed when no trusted binary is
// registered: a repository can influence the hook's PATH, so an unverified
// resolution must not run. It still exits without blocking the session.
func TestMemoryPluginHookFailsClosedInsideRepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh script behavior test")
	}
	repo := t.TempDir()
	git, gitErr := exec.LookPath("git")
	if gitErr != nil {
		t.Skip("git unavailable for repository detection")
	}
	require.NoError(t, exec.CommandContext(t.Context(), git, "-C", repo, "init", "-q").Run(), "git init")

	cmd := exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(
		memoryPluginRoot(t), "scripts", "session-start.sh"))
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + repo,
		"AGENTSVIEW_DATA_DIR=" + filepath.Join(repo, "injected"),
	}
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(output), "no registered binary path",
		"an unregistered hook must fail closed inside a repository")
	assert.NotContains(t, string(output), filepath.Join(repo, "injected"),
		"a repository-controlled data-dir override must not be followed")
}

// A registered binary that itself lives inside a repository is refused even
// when the hook runs outside that repository.
func TestMemoryPluginHookRefusesRepoLocalRegisteredBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh script behavior test")
	}
	repo := t.TempDir()
	git, gitErr := exec.LookPath("git")
	if gitErr != nil {
		t.Skip("git unavailable for repository detection")
	}
	require.NoError(t, exec.CommandContext(t.Context(), git, "-C", repo,
		"init", "-q").Run(), "git init")
	binDir := filepath.Join(repo, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	bin := filepath.Join(binDir, "agentsview")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho IMPOSTOR\n"), 0o755))

	recordHome := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(recordHome, ".agentsview"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(recordHome, ".agentsview", "registered-bin"),
		[]byte(bin), 0o600))

	outside := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(
		memoryPluginRoot(t), "scripts", "session-start.sh"))
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + recordHome,
	}
	cmd.Dir = outside
	output, err := cmd.CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(output), "refusing registered binary inside a repository")
	assert.NotContains(t, string(output), "IMPOSTOR")
}

// The MCP launcher prefers the registered absolute binary and passes the
// focused memory profile; without a registration it falls back to PATH.
func TestMemoryPluginMCPLauncherResolvesRegisteredBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh script behavior test")
	}
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "agentsview")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho \"REGISTERED:$*\"\n"), 0o755))
	recordHome := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(recordHome, ".agentsview"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(recordHome, ".agentsview", "registered-bin"),
		[]byte(bin), 0o600))

	cmd := exec.CommandContext(t.Context(), "/bin/sh", filepath.Join(
		memoryPluginRoot(t), "scripts", "mcp-server.sh"))
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + recordHome}
	output, err := cmd.CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(output), "REGISTERED:mcp --profile memory")
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
	// The launcher script resolves a trusted binary and passes the focused
	// profile itself; the manifest must not fall back to a bare PATH command.
	assert.Contains(t, string(mcp), "${CLAUDE_PLUGIN_ROOT}/scripts/mcp-server.sh")
	assert.NotContains(t, string(mcp), "AGENTSVIEW_SERVER_TOKEN\"")
	wrapper, wrapperErr := os.ReadFile(filepath.Join(
		root, "scripts", "mcp-server.sh"))
	require.NoError(t, wrapperErr, "read mcp-server.sh")
	assert.Contains(t, string(wrapper), "mcp --profile memory",
		"the launcher must pass the focused memory profile")
}
