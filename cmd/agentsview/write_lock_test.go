package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func writeDBConfigForTest(t *testing.T) (string, config.Config) {
	t.Helper()
	dataDir := t.TempDir()
	return dataDir, config.Config{
		DataDir: dataDir,
		DBPath:  filepath.Join(dataDir, "sessions.db"),
	}
}

func acquireWriteOwnerLockForTest(
	t *testing.T,
	dataDir string,
) *writeOwnerLock {
	t.Helper()
	lock, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(t, err)
	return lock
}

func holdWriteOwnerLockForTest(
	t *testing.T,
	dataDir string,
) *writeOwnerLock {
	t.Helper()
	lock := acquireWriteOwnerLockForTest(t, dataDir)
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	return lock
}

func assertOpenWriteDBRefused(
	t *testing.T,
	cfg config.Config,
	wantSubstrings ...string,
) {
	t.Helper()
	database, lock, err := openWriteDB(context.Background(), cfg)
	require.Error(t, err)
	require.Nil(t, database)
	require.Nil(t, lock)
	assertErrorContainsAll(t, err, wantSubstrings...)
}

func requireOpenWriteDBForTest(
	t *testing.T,
	cfg config.Config,
) (*db.DB, *writeOwnerLock) {
	t.Helper()
	database, lock, err := openWriteDB(context.Background(), cfg)
	require.NoError(t, err)
	require.NotNil(t, database)
	require.NotNil(t, lock)
	return database, lock
}

func holdBackgroundLaunchLockForTest(t *testing.T, dataDir string) *flock.Flock {
	t.Helper()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	launchLock, ok := acquireBackgroundLaunchLock(dataDir)
	require.True(t, ok)
	t.Cleanup(func() { require.NoError(t, launchLock.Unlock()) })
	return launchLock
}

func holdExternalStartupLockForTest(t *testing.T, dataDir string) *flock.Flock {
	t.Helper()
	lockPath, err := runtimeStore(dataDir).LockPath()
	require.NoError(t, err)
	startLock := flock.New(lockPath)
	locked, err := startLock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	t.Cleanup(func() { require.NoError(t, startLock.Unlock()) })
	return startLock
}

func assertErrorContainsAll(
	t *testing.T,
	err error,
	substrings ...string,
) {
	t.Helper()
	require.Error(t, err)
	for _, substr := range substrings {
		assert.Contains(t, err.Error(), substr)
	}
}

func TestWriteOwnerLockPathUsesDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "agentsview-data")

	assert.Equal(
		t,
		filepath.Join(dataDir, "db.write.lock"),
		writeOwnerLockPath(dataDir),
	)
}

func TestAcquireWriteOwnerLockExcludesSecondOwner(t *testing.T) {
	dataDir := t.TempDir()

	first, err := acquireWriteOwnerLock(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })

	second, err := tryAcquireWriteOwnerLock(dataDir)
	require.Error(t, err)
	require.Nil(t, second)
	assertErrorContainsAll(t, err, writeOwnerLockPath(dataDir),
		"agentsview serve stop")
}

func TestWriteOwnerLockReleaseAllowsNextOwner(t *testing.T) {
	dataDir := t.TempDir()

	first := acquireWriteOwnerLockForTest(t, dataDir)
	require.NoError(t, first.Close())

	second := acquireWriteOwnerLockForTest(t, dataDir)
	require.NoError(t, second.Close())
}

func TestWriteOwnerLockErrorMentionsServeStop(t *testing.T) {
	dataDir := t.TempDir()

	holdWriteOwnerLockForTest(t, dataDir)

	_, err := tryAcquireWriteOwnerLock(dataDir)
	assertErrorContainsAll(t, err, "agentsview serve stop", "idle timeout")
}

func TestOpenWriteDBRefusesSecondOwner(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)

	holdWriteOwnerLockForTest(t, dataDir)

	assertOpenWriteDBRefused(t, cfg, writeOwnerLockPath(dataDir))
}

func TestOpenWriteDBReleasesOwnerAfterClose(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)

	database, lock := requireOpenWriteDBForTest(t, cfg)
	closeWriteDB(database, lock)

	next := acquireWriteOwnerLockForTest(t, dataDir)
	require.NoError(t, next.Close())
}

func TestOpenWriteDBRefusesLiveWritableRuntime(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	_, err := WriteDaemonRuntime(dataDir, "127.0.0.1", 9, "test", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dataDir) })

	assertOpenWriteDBRefused(t, cfg, "refusing to write directly",
		"agentsview serve stop")
}

func TestOpenWriteDBAllowsStartupLockHeldByCurrentServe(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	MarkDaemonStarting(dataDir)
	t.Cleanup(func() { UnmarkDaemonStarting(dataDir) })

	database, lock := requireOpenWriteDBForTest(t, cfg)
	closeWriteDB(database, lock)
}

func TestOpenWriteDBRefusesExternalStartupLock(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	holdExternalStartupLockForTest(t, dataDir)

	assertOpenWriteDBRefused(t, cfg, "daemon is starting",
		"refusing to write directly")
}

func TestOpenWriteDBRefusesBackgroundLaunchLock(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	holdBackgroundLaunchLockForTest(t, dataDir)

	assertOpenWriteDBRefused(t, cfg, "daemon launch is in progress",
		"refusing to write directly")
}

func TestOpenWriteDBAllowsBackgroundChildWithLaunchLock(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	t.Setenv(backgroundChildEnvVar, "1")
	holdBackgroundLaunchLockForTest(t, dataDir)

	database, lock := requireOpenWriteDBForTest(t, cfg)
	closeWriteDB(database, lock)
}
