//go:build !windows

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReplicaWatchLifecyclePublishesAndDeliversWake(t *testing.T) {
	dir := t.TempDir()
	wake, cleanup, err := installReplicaWatchLifecycle(
		dir, "pg", "team",
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	require.NoError(t, notifyReplicaWatchLifecycle(
		t.Context(), dir, "pg", "team",
	))
	select {
	case <-wake:
	case <-time.After(time.Second):
		require.FailNow(t, "replica watch owner did not receive lifecycle wake")
	}
}
