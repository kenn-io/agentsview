package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplicaWatchRefusesHeldLegacyLock(t *testing.T) {
	dir := t.TempDir()
	isolateDeploymentEnv(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	output := log.Writer()
	t.Cleanup(func() {
		watchLog := log.Writer()
		log.SetOutput(output)
		if f, ok := watchLog.(*os.File); ok && watchLog != output {
			require.NoError(t, f.Close())
		}
	})
	writeTestConfig(t, dir, `
[pg]
url = "postgres://user@127.0.0.1:1/archive"
allow_insecure = true
`)
	lockPath := filepath.Join(dir, "pg-watch.lock")
	lock := flock.New(lockPath)
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod(dir, 0o755))
	}

	err := runReplicaPushWatch(pgReplica{}, ReplicaPushConfig{}, "")
	require.EqualError(t, err, "already locked ("+lockPath+")")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	}
}
