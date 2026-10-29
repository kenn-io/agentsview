package main

import (
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
)

func TestReplicaWatchLockLeavesDataDirUntouched(t *testing.T) {
	dir, lockPath := replicaWatchLockFixture(t)
	check := replicaWatchDataDirCheck(t, dir)

	err := runReplicaPushWatch(pgReplica{}, ReplicaPushConfig{}, "")
	require.EqualError(t, err, "already locked ("+lockPath+")")
	check()
}

func replicaWatchLockFixture(t *testing.T) (string, string) {
	t.Helper()
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
	return dir, lockPath
}
