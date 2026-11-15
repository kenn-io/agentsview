//go:build fts5 && linux

package db

import (
	"os"
	"os/exec"
	"os/signal"
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
		id := os.Getenv("USAGE_UPGRADE_LIMIT_ID")
		cache, err := manager.Generation(t.Context(), id)
		require.NoError(t, err)
		assert.True(t, cache.temporary)
		assert.FileExists(t, usageCacheGenerationPath(archive, 23, id))
		assert.NoFileExists(t, usageCacheGenerationPath(archive, 24, id))
		return
	}
	archive, old, id := copyUsageV23Fixture(t)
	cache := openRawUsageCacheTestDB(t, old)
	_, err := cache.ExecContext(t.Context(), `CREATE TABLE padding (data BLOB); INSERT INTO padding VALUES (zeroblob(4194304))`)
	require.NoError(t, err)
	require.NoError(t, cache.Close())
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestUsageCacheUpgradeCopyFailure$")
	cmd.Env = append(os.Environ(), "USAGE_UPGRADE_LIMIT_ARCHIVE="+archive, "USAGE_UPGRADE_LIMIT_ID="+id)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.FileExists(t, old)
	assert.NoFileExists(t, usageCacheGenerationPath(archive, 24, id))
	manager := newUsageCacheManager(archive)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	upgraded, err := manager.Generation(t.Context(), id)
	require.NoError(t, err)
	assert.False(t, upgraded.temporary)
	assert.NoFileExists(t, old)
}
