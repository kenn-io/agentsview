package pidlock

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquire_FreshSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg-watch.lock")
	lock, err := Acquire(path)
	require.NoError(t, err, "Acquire")
	defer func() { require.NoError(t, lock.Release(), "Release") }()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
}

func TestAcquire_LiveHolderFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg-watch.lock")
	lock, err := Acquire(path)
	require.NoError(t, err, "Acquire")
	defer func() { require.NoError(t, lock.Release(), "Release") }()

	_, err = Acquire(path)
	require.Error(t, err, "expected Acquire to fail when a live holder exists")
	assert.Contains(t, err.Error(), "already locked")
}

func TestAcquire_StalePIDReclaimed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg-watch.lock")
	// PID 2147483647 is not a running process.
	require.NoError(t, os.WriteFile(path, []byte("2147483647"), 0o644))
	lock, err := Acquire(path)
	require.NoError(t, err, "Acquire should reclaim a stale lock")
	defer func() { require.NoError(t, lock.Release(), "Release") }()
}

func TestRelease_UnlocksAndKeepsMarkerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg-watch.lock")
	lock, err := Acquire(path)
	require.NoError(t, err, "Acquire")
	require.NoError(t, lock.Release(), "Release")

	_, err = os.Stat(path)
	require.NoError(t, err, "PID marker file should remain")

	lock, err = Acquire(path)
	require.NoError(t, err, "Acquire after Release")
	defer func() { require.NoError(t, lock.Release(), "Release") }()
}

func TestAcquire_UnparseableLockReclaimed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg-watch.lock")
	require.NoError(t, os.WriteFile(path, []byte("not-a-pid\n"), 0o644))
	lock, err := Acquire(path)
	require.NoError(t, err, "Acquire should reclaim an unparseable lock")
	defer func() { require.NoError(t, lock.Release(), "Release") }()
}

func TestRelease_NilReceiverNoError(t *testing.T) {
	var l *Lock
	require.NoError(t, l.Release())
}
