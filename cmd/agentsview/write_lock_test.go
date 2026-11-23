package main

import (
	"bytes"
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
) error {
	t.Helper()

	database, lock, err := openWriteDB(t.Context(), cfg)
	require.Error(t, err)
	require.Nil(t, database)
	require.Nil(t, lock)
	assertErrorContainsAll(t, err, wantSubstrings...)
	return err
}

func requireOpenWriteDBForTest(
	t *testing.T,
	cfg config.Config,
) (*db.DB, *writeOwnerLock) {
	t.Helper()

	database, lock, err := openWriteDB(t.Context(), cfg)
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

	first, err := acquireWriteOwnerLock(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })

	second, err := tryAcquireWriteOwnerLock(dataDir)
	require.Error(t, err)
	require.Nil(t, second)
	assertErrorContainsAll(t, err, writeOwnerLockPath(dataDir),
		"agentsview daemon stop", "agentsview daemon status",
		"find the agentsview process holding "+writeOwnerLockPath(dataDir))
}

func TestWriteOwnerLockReleaseAllowsNextOwner(t *testing.T) {
	dataDir := t.TempDir()

	first := acquireWriteOwnerLockForTest(t, dataDir)
	require.NoError(t, first.Close())

	second := acquireWriteOwnerLockForTest(t, dataDir)
	require.NoError(t, second.Close())
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
		"agentsview daemon stop")
}

func TestDirectWriteRechecksDaemonAfterLock(t *testing.T) {
	for _, path := range []string{"open", "classifier", "recovery"} {
		t.Run(path, func(t *testing.T) {
			dataDir, cfg := writeDBConfigForTest(t)
			if path == "classifier" {
				seedClassifierHash(t, cfg)
			}
			manifest := filepath.Join(dataDir, "compact-recovery.json")
			if path == "recovery" {
				require.NoError(t, os.WriteFile(manifest, []byte("pending"), 0o600))
			}
			original := acquireDirectWriteOwnerLock
			acquireDirectWriteOwnerLock = func(ctx context.Context, dir string) (*writeOwnerLock, error) {
				lock, err := original(ctx, dir)
				if err != nil {
					return nil, err
				}
				_, err = WriteDaemonRuntime(dataDir, "127.0.0.1", 9, "test", false)
				if err != nil {
					lock.Close()
					return nil, err
				}
				return lock, nil
			}
			t.Cleanup(func() { acquireDirectWriteOwnerLock = original; RemoveDaemonRuntime(dataDir) })
			var err error
			switch path {
			case "open":
				opened := false
				_, _, err = openWriteDBWith(t.Context(), cfg, func(context.Context, config.Config) (*db.DB, error) {
					opened = true
					return nil, nil
				})
				assert.False(t, opened)
			case "classifier":
				err = runClassifierRebuild(t.Context(), cfg, &bytes.Buffer{}, false)
				assert.NotEmpty(t, classifierHashInSQLite(t, cfg.DBPath))
			case "recovery":
				_, err = archiveModeAfterRecovery(t.Context(), cfg)
				content, readErr := os.ReadFile(manifest)
				require.NoError(t, readErr)
				assert.Equal(t, "pending", string(content))
			}
			require.ErrorContains(t, err, "refusing to write directly")
			lock := acquireWriteOwnerLockForTest(t, dataDir)
			require.NoError(t, lock.Close())
		})
	}
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

	err := assertOpenWriteDBRefused(t, cfg)
	assert.EqualError(t, err,
		"local daemon is starting and owns the SQLite archive; refusing to "+
			"write directly. Retry once it is ready")
}

func TestOpenWriteDBRefusesBackgroundLaunchLock(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	holdBackgroundLaunchLockForTest(t, dataDir)

	err := assertOpenWriteDBRefused(t, cfg)
	assert.EqualError(t, err,
		"local daemon launch is in progress and owns the SQLite archive; "+
			"refusing to write directly. Retry once it is ready")
}

func TestOpenWriteDBAllowsBackgroundChildWithLaunchLock(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	t.Setenv(backgroundChildEnvVar, "1")
	holdBackgroundLaunchLockForTest(t, dataDir)

	database, lock := requireOpenWriteDBForTest(t, cfg)
	closeWriteDB(database, lock)
}
