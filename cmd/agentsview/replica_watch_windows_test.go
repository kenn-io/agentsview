//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplicaWatchLockPreservesArchiveChangeTime(t *testing.T) {
	dir, lockPath := replicaWatchLockFixture(t)
	path := filepath.Join(dir, "sentinel")
	require.NoError(t, os.WriteFile(path, []byte("archive content"), 0o600))
	before := fileChangeTime(t, path)

	err := runReplicaPushWatch(pgReplica{}, ReplicaPushConfig{}, "")
	require.EqualError(t, err, "already locked ("+lockPath+")")
	after := fileChangeTime(t, path)
	assert.Equal(t, before, after, "%s ChangeTime changed", path)
}
