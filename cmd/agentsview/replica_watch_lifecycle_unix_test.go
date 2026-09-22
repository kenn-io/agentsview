//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
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

// rewriteWatchLifecycleRecord replaces the stored lifecycle record for pid
// with one whose process identity fields and metadata are chosen by the
// test, simulating records left behind by older binaries or by a process
// whose PID was later reused.
func rewriteWatchLifecycleRecord(
	t *testing.T, dir, backend, target string,
	clearIdentity bool, createTime string,
) {
	t.Helper()
	store := daemon.RuntimeStore{Dir: dir, Prefix: backend + "-watch"}
	records, err := store.List()
	require.NoError(t, err)
	service := "agentsview-" + backend + "-watch"
	var found *daemon.RuntimeRecord
	for i := range records {
		rec := records[i]
		if rec.Service == service && rec.Metadata[replicaWatchLifecycleTarget] == target {
			found = &records[i]
			break
		}
	}
	require.NotNil(t, found, "lifecycle record not found")
	if clearIdentity {
		found.ProcessIdentity = ""
		found.ProcessIdentityV2 = ""
	}
	if found.Metadata == nil {
		found.Metadata = map[string]string{}
	}
	if createTime == "" {
		delete(found.Metadata, runtimeCreateTime)
	} else {
		found.Metadata[runtimeCreateTime] = createTime
	}
	_, err = store.Write(*found)
	require.NoError(t, err)
}

// A stale record whose create time no longer matches the live process
// holding the recorded PID must be treated as a reused PID: notify refuses
// to signal it and reports the owner as not running.
func TestReplicaWatchLifecycleRejectsReusedPID(t *testing.T) {
	dir := t.TempDir()
	wake, cleanup, err := installReplicaWatchLifecycle(dir, "pg", "team")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	rewriteWatchLifecycleRecord(t, dir, "pg", "team", false, "123")

	err = notifyReplicaWatchLifecycle(t.Context(), dir, "pg", "team")
	require.ErrorContains(t, err, "owner is not running")
	select {
	case <-wake:
		require.FailNow(t, "reused PID received lifecycle wake")
	case <-time.After(50 * time.Millisecond):
	}
}

// A stale record left by a crashed watcher (dead PID, older StartedAt so it
// sorts ahead of the live owner) must not stop notify from finding and
// waking the live record listed after it.
func TestReplicaWatchLifecycleSkipsStaleRecordBeforeLiveOwner(t *testing.T) {
	dir := t.TempDir()
	wake, cleanup, err := installReplicaWatchLifecycle(dir, "pg", "team")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	dead := exec.CommandContext(t.Context(), "sh", "-c", "exit 0")
	require.NoError(t, dead.Start())
	deadPID := dead.Process.Pid
	require.NoError(t, dead.Wait())
	require.False(t, daemon.ProcessAlive(deadPID), "test PID must be dead")

	store := daemon.RuntimeStore{Dir: dir, Prefix: "pg-watch"}
	_, err = store.Write(daemon.RuntimeRecord{
		PID:       deadPID,
		Network:   "signal",
		Address:   strconv.Itoa(deadPID),
		Service:   "agentsview-pg-watch",
		Version:   version,
		StartedAt: time.Now().UTC().Add(-time.Minute),
		Metadata: map[string]string{
			replicaWatchLifecycleTarget: "team",
		},
	})
	require.NoError(t, err)

	require.NoError(t, notifyReplicaWatchLifecycle(t.Context(), dir, "pg", "team"))
	select {
	case <-wake:
	case <-time.After(time.Second):
		require.FailNow(t, "replica watch owner did not receive lifecycle wake")
	}
}

// A record whose identity cannot be verified at all (no kit identity and no
// create time) is treated as unavailable: with a reused PID there would be no
// signal to distinguish the owner from an unrelated process, so notify must
// not signal it.
func TestReplicaWatchLifecycleRejectsRecordWithoutIdentity(t *testing.T) {
	dir := t.TempDir()
	wake, cleanup, err := installReplicaWatchLifecycle(dir, "duckdb", "team")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	rewriteWatchLifecycleRecord(t, dir, "duckdb", "team", true, "")

	err = notifyReplicaWatchLifecycle(t.Context(), dir, "duckdb", "team")
	require.ErrorContains(t, err, "owner is not running")
	select {
	case <-wake:
		require.FailNow(t, "unverifiable record received lifecycle wake")
	case <-time.After(50 * time.Millisecond):
	}
}

// A record with no kit identity but a matching create time resolves its
// owner through the create-time fallback.
func TestReplicaWatchLifecycleCreateTimeFallback(t *testing.T) {
	dir := t.TempDir()
	wake, cleanup, err := installReplicaWatchLifecycle(dir, "clickhouse", "team")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	ct, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok, "live create time must be readable for this test")
	rewriteWatchLifecycleRecord(
		t, dir, "clickhouse", "team", true, strconv.FormatInt(ct, 10),
	)

	require.NoError(t, notifyReplicaWatchLifecycle(t.Context(), dir, "clickhouse", "team"))
	select {
	case <-wake:
	case <-time.After(time.Second):
		require.FailNow(t, "replica watch owner did not receive lifecycle wake")
	}
}
