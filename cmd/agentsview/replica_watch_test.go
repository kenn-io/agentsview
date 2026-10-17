package main

import (
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplicaPushWatchHonorsExistingLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_PG_URL", "")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
		"[pg]\nurl = 'postgres://user:pass@127.0.0.1/archive?sslmode=disable'\nmachine_name = 'host-a'\n",
	), 0o600))
	previousOutput := log.Writer()
	t.Cleanup(func() {
		if file, ok := log.Writer().(*os.File); ok && file != previousOutput {
			assert.NoError(t, file.Close())
		}
		log.SetOutput(previousOutput)
	})
	path := filepath.Join(dir, "pg-watch.lock")
	lock := flock.New(path)
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	t.Cleanup(func() { assert.NoError(t, lock.Unlock()) })

	err = runReplicaPushWatch(pgReplica{}, ReplicaPushConfig{}, "")
	require.EqualError(t, err, "already locked ("+path+")")
}
