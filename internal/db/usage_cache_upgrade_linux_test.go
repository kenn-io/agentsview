//go:build fts5 && linux

package db

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageCacheUpgradeCopyFailure(t *testing.T) {
	if archive := os.Getenv("USAGE_UPGRADE_LIMIT_ARCHIVE"); archive != "" {
		signal.Ignore(syscall.SIGXFSZ)
		require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 1 << 20, Max: 1 << 20}))
		manager := newUsageCacheManager(archive)
		defer manager.Close()
		cache, err := manager.Generation(t.Context(), "id")
		require.NoError(t, err)
		assert.True(t, cache.temporary)
		assert.FileExists(t, usageCacheGenerationPath(archive, 23, "id"))
		assert.NoFileExists(t, usageCacheGenerationPath(archive, 24, "id"))
		return
	}
	archive := filepath.Join(t.TempDir(), "archive.db")
	old := usageCacheGenerationPath(archive, 23, "id")
	cache, err := initializeUsageCache(t.Context(), old, "id", false)
	require.NoError(t, err)
	_, err = cache.db.ExecContext(t.Context(), `CREATE TABLE padding (data BLOB); INSERT INTO padding VALUES (zeroblob(4194304))`)
	require.NoError(t, err)
	require.NoError(t, cache.db.Close())
	makeFormat23Cache(t, old)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUsageCacheUpgradeCopyFailure$")
	cmd.Env = append(os.Environ(), "USAGE_UPGRADE_LIMIT_ARCHIVE="+archive)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.FileExists(t, old)
	assert.NoFileExists(t, usageCacheGenerationPath(archive, 24, "id"))
	manager := newUsageCacheManager(archive)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	upgraded, err := manager.Generation(t.Context(), "id")
	require.NoError(t, err)
	assert.False(t, upgraded.temporary)
	assert.NoFileExists(t, old)
}
