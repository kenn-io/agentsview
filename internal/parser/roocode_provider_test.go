package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeRooCodeDiscoveryTask(t *testing.T, root, taskID string) string {
	t.Helper()
	taskDir := filepath.Join(root, "tasks", taskID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))
	historyPath := filepath.Join(taskDir, "history_item.json")
	require.NoError(t, os.WriteFile(
		historyPath, []byte(`{"id":"`+taskID+`"}`), 0o644,
	))
	return historyPath
}

func rooCodeDiscoverPaths(t *testing.T, provider Provider) ([]string, error) {
	t.Helper()
	discoverer, ok := provider.(StreamingDiscoverer)
	require.True(t, ok)
	var paths []string
	err := discoverer.DiscoverEach(t.Context(), func(source SourceRef) error {
		paths = append(paths, source.DisplayPath)
		return nil
	})
	return paths, err
}

// TestRooCodeDiscoveryUnreadableTasksDirFails guards the reconciliation
// tombstoning path: discovery used to convert every os.ReadDir failure on
// <root>/tasks into an empty successful enumeration, so a transient
// permission or I/O failure made the provider-authoritative scope look
// empty and the engine tombstoned every baselined RooCode session under it.
// Traversal failures must propagate so the failed scope stays incomplete.
func TestRooCodeDiscoveryUnreadableTasksDirFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory-permission read failures are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	root := t.TempDir()
	historyPath := writeRooCodeDiscoveryTask(t, root, "task-1")
	tasksDir := filepath.Join(root, "tasks")

	provider, ok := NewProvider(AgentRooCode, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	paths, err := rooCodeDiscoverPaths(t, provider)
	require.NoError(t, err)
	require.Equal(t, []string{historyPath}, paths,
		"a readable tasks directory must enumerate its sessions")

	require.NoError(t, os.Chmod(tasksDir, 0o000))
	t.Cleanup(func() {
		require.NoError(t, os.Chmod(tasksDir, 0o755))
	})

	paths, err = rooCodeDiscoverPaths(t, provider)
	require.Error(t, err,
		"an unreadable tasks directory must not stream an authoritative empty discovery")
	assert.ErrorIs(t, err, os.ErrPermission)
	assert.Empty(t, paths)

	_, err = provider.Discover(t.Context())
	require.Error(t, err,
		"an unreadable tasks directory must not collect an authoritative empty discovery")
}

// A root without a tasks directory is a legitimately empty scope, not a
// failure: discovery completes with no sources and no error.
func TestRooCodeDiscoveryMissingTasksDirIsEmptyComplete(t *testing.T) {
	provider, ok := NewProvider(AgentRooCode, ProviderConfig{
		Roots: []string{t.TempDir()},
	})
	require.True(t, ok)

	paths, err := rooCodeDiscoverPaths(t, provider)
	require.NoError(t, err)
	assert.Empty(t, paths)
}

// Discovery skips non-directory entries, underscore-prefixed metadata
// directories, and task directories without a history_item.json.
func TestRooCodeDiscoverySkipsNonSessionEntries(t *testing.T) {
	root := t.TempDir()
	historyPath := writeRooCodeDiscoveryTask(t, root, "task-1")
	tasksDir := filepath.Join(root, "tasks")
	require.NoError(t, os.MkdirAll(filepath.Join(tasksDir, "_index"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tasksDir, "no-history"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tasksDir, "stray.json"), []byte("{}"), 0o644,
	))

	provider, ok := NewProvider(AgentRooCode, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	paths, err := rooCodeDiscoverPaths(t, provider)
	require.NoError(t, err)
	assert.Equal(t, []string{historyPath}, paths)
}
