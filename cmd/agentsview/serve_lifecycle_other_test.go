//go:build !linux

package main

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestStopDaemonProcessHandlesUnknownIdentityAfterForceKill(
	t *testing.T,
) {
	requirePOSIXSignals(t, "requires POSIX signals and signal-zero zombie liveness")
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	pid := startTERMIgnoringProcess(t)
	createTime, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	recorded := strconv.FormatInt(createTime, 10)
	path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:      pid,
		Network:  daemon.NetworkTCP,
		Address:  "127.0.0.1:1",
		Metadata: map[string]string{runtimeCreateTime: recorded},
	})
	require.NoError(t, err)
	rec := onlyLiveRuntimeRecord(t, dir)

	identityCalls := 0
	identityState := func(got daemon.RuntimeRecord) processCreateTimeState {
		assert.Equal(t, pid, got.PID)
		assert.Equal(t, recorded, got.Metadata[runtimeCreateTime])
		identityCalls++
		if identityCalls <= 2 {
			return processCreateTimeMatch
		}
		return processCreateTimeUnknown
	}

	err = stopDaemonProcessWithIdentity(
		rec, 50*time.Millisecond, identityState,
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "identity")
	require.ErrorContains(t, err, "after force kill")
	assert.Equal(t, 3, identityCalls)
	assert.FileExists(t, path,
		"unknown post-kill identity must preserve the runtime record")
}

func TestStopDaemonProcessHandlesMatchedIdentityAfterForceKill(
	t *testing.T,
) {
	requirePOSIXSignals(t, "requires POSIX signals and signal-zero zombie liveness")
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	pid := startTERMIgnoringProcess(t)
	createTime, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	recorded := strconv.FormatInt(createTime, 10)
	path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:      pid,
		Network:  daemon.NetworkTCP,
		Address:  "127.0.0.1:1",
		Metadata: map[string]string{runtimeCreateTime: recorded},
	})
	require.NoError(t, err)
	rec := onlyLiveRuntimeRecord(t, dir)

	identityCalls := 0
	identityState := func(got daemon.RuntimeRecord) processCreateTimeState {
		assert.Equal(t, pid, got.PID)
		assert.Equal(t, recorded, got.Metadata[runtimeCreateTime])
		identityCalls++
		return processCreateTimeMatch
	}

	err = stopDaemonProcessWithIdentity(
		rec, 50*time.Millisecond, identityState,
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "still running after force kill")
	assert.Equal(t, 3, identityCalls,
		"identity must be confirmed before SIGTERM, before SIGKILL, and after SIGKILL")
	assert.FileExists(t, path,
		"surviving matched process must retain its ownership record")
}
