//go:build linux

package main

import (
	"bytes"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/daemon"
)

func writeClockShiftedRuntimeRecord(
	t *testing.T, dir string, endpoint testDaemonEndpoint,
) (string, daemon.RuntimeRecord) {
	t.Helper()
	path, err := WriteDaemonRuntime(
		dir, endpoint.Host, endpoint.Port, "test", false,
	)
	require.NoError(t, err)
	rec, err := runtimeStore(dir).Read(path)
	require.NoError(t, err)
	require.NotEmpty(t, rec.ProcessIdentityV2)
	createTime, err := strconv.ParseInt(rec.Metadata[runtimeCreateTime], 10, 64)
	require.NoError(t, err)
	rec.Metadata[runtimeCreateTime] = strconv.FormatInt(createTime+30000, 10)
	_, err = runtimeStore(dir).Write(rec)
	require.NoError(t, err)
	rec.SourcePath = path
	return path, rec
}

func TestDaemonStatusKeepsClockShiftedLiveDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	endpoint := newPingDaemon(t)
	path, rec := writeClockShiftedRuntimeRecord(t, dir, endpoint)

	deps := defaultDaemonCommandDeps()
	deps.loadReadOnlyConfig = func() (config.Config, error) {
		return config.Config{DataDir: dir}, nil
	}
	deps.statusRecords = daemonStatusRecords
	var out bytes.Buffer
	require.NoError(t, runDaemonStatus(&out, deps))
	assert.Contains(t, out.String(), "agentsview running at")
	assert.Contains(t, out.String(), strconv.Itoa(rec.PID))
	assert.FileExists(t, path)

	rt := FindDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.Equal(t, rec.PID, rt.Record.PID)
}

func TestClockShiftedRuntimeRecordStaysVisibleToDiscovery(t *testing.T) {
	tests := []struct {
		name  string
		check func(*testing.T, string, daemon.RuntimeRecord)
	}{
		{
			name: "find daemon",
			check: func(t *testing.T, dir string, rec daemon.RuntimeRecord) {
				t.Helper()
				rt := FindDaemonRuntime(dir)
				require.NotNil(t, rt)
				assert.Equal(t, rec.PID, rt.Record.PID)
			},
		},
		{
			name: "find incompatible daemon",
			check: func(t *testing.T, dir string, rec daemon.RuntimeRecord) {
				t.Helper()
				rec.Metadata[runtimeAPIVersion] = "0"
				_, err := runtimeStore(dir).Write(rec)
				require.NoError(t, err)
				rt, err := FindIncompatibleDaemonRuntime(dir)
				require.Error(t, err)
				require.NotNil(t, rt)
				assert.Equal(t, rec.PID, rt.Record.PID)
			},
		},
		{
			name: "writable records",
			check: func(t *testing.T, dir string, rec daemon.RuntimeRecord) {
				t.Helper()
				records, err := writableDaemonRecordsFromStore(runtimeStore(dir))
				require.NoError(t, err)
				require.Len(t, records, 1)
				assert.Equal(t, rec.PID, records[0].PID)
			},
		},
		{
			name: "writable records with fallback",
			check: func(t *testing.T, _ string, rec daemon.RuntimeRecord) {
				t.Helper()
				resolved := false
				records, fallback := writableDaemonRecordsWithFallback(
					[]daemon.RuntimeRecord{rec}, func() *DaemonRuntime {
						resolved = true
						return nil
					},
				)
				require.Len(t, records, 1)
				assert.Equal(t, rec.PID, records[0].PID)
				assert.False(t, fallback)
				assert.False(t, resolved)
			},
		},
		{
			name: "live daemon",
			check: func(t *testing.T, dir string, _ daemon.RuntimeRecord) {
				t.Helper()
				assert.True(t, hasLiveDaemonRuntime(dir))
			},
		},
		{
			name: "live writable daemon",
			check: func(t *testing.T, dir string, _ daemon.RuntimeRecord) {
				t.Helper()
				assert.True(t, hasLiveWritableDaemonRuntime(dir))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := runtimeTestDir(t)
			endpoint := newPingDaemon(t)
			_, rec := writeClockShiftedRuntimeRecord(t, dir, endpoint)
			tt.check(t, dir, rec)
		})
	}
}

func TestPresentInvalidV2PreservesRuntimeRecord(t *testing.T) {
	dir := runtimeTestDir(t)
	endpoint := newPingDaemon(t)
	path, rec := writeClockShiftedRuntimeRecord(t, dir, endpoint)
	rec.ProcessIdentityV2 = "future-v1:12345"
	_, err := runtimeStore(dir).Write(rec)
	require.NoError(t, err)

	records, err := daemonStatusRecords(dir, "")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, rec.PID, records[0].PID)
	assert.FileExists(t, path)

	rt := FindDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.Equal(t, rec.PID, rt.Record.PID)
}

func TestStopTargetConfirmedHungDaemonAfterClockStep(t *testing.T) {
	identity, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	rec := daemon.RuntimeRecord{
		PID:               os.Getpid(),
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:1",
		Service:           daemonService,
		ProcessIdentityV2: identity,
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(createTime+30000, 10),
		},
	}

	assert.False(t, daemonRecordPingConfirmed(rec, ""))
	assert.True(t, stopTargetConfirmed(rec, ""))
}

func TestStopTargetConfirmedRejectsMismatchedV2(t *testing.T) {
	pid := startSleepProcess(t)
	identity, ok := daemon.ReadProcessIdentity(pid)
	require.True(t, ok)
	createTime, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	rec := daemon.RuntimeRecord{
		PID:               pid,
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:1",
		Service:           daemonService,
		ProcessIdentityV2: mismatchedProcessIdentityForTest(t, identity),
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(createTime, 10),
		},
	}

	assert.False(t, daemonRecordPingConfirmed(rec, ""))
	assert.False(t, stopTargetConfirmed(rec, ""))
}

func TestStopTargetConfirmedMatchingPingBypassesIdentityState(t *testing.T) {
	endpoint := newPingDaemonWithPID(t, os.Getpid())
	rec := daemon.RuntimeRecord{
		PID:               os.Getpid(),
		Network:           daemon.NetworkTCP,
		Address:           endpoint.Addr,
		Service:           daemonService,
		ProcessIdentityV2: "future-v1:12345",
	}

	assert.True(t, daemonRecordPingConfirmed(rec, ""))
	assert.True(t, stopTargetConfirmed(rec, ""),
		"a matching ping remains an independent initial-stop route")
}

func TestStopDaemonProcessForceKillsHungDaemonAfterClockStep(t *testing.T) {
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	pid, reaped := startReapedTERMIgnoringProcess(t)
	identity, ok := daemon.ReadProcessIdentity(pid)
	require.True(t, ok)
	createTime, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:               pid,
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:1",
		ProcessIdentityV2: identity,
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(createTime+30000, 10),
		},
	})
	require.NoError(t, err)

	require.NoError(t, stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), 50*time.Millisecond))
	<-reaped
	assert.False(t, daemon.ProcessAlive(pid))
	assert.NoFileExists(t, path)
}

func TestStopDaemonProcessMismatchedV2DoesNotForceKill(t *testing.T) {
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	pid, reaped := startReapedTERMIgnoringProcess(t)
	identity, ok := daemon.ReadProcessIdentity(pid)
	require.True(t, ok)
	createTime, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	mismatched := mismatchedProcessIdentityForTest(t, identity)
	path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:               pid,
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:1",
		ProcessIdentityV2: mismatched,
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(createTime, 10),
		},
	})
	require.NoError(t, err)

	require.NoError(t, stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), 50*time.Millisecond))
	assert.True(t, daemon.ProcessAlive(pid))
	assert.NoFileExists(t, path)
	select {
	case <-reaped:
		assert.Fail(t, "mismatched v2 identity must not force-kill the process")
	default:
	}
}

func TestStopDaemonProcessUnknownV2DoesNotForceKill(t *testing.T) {
	setStartProbeTickForTest(t, 10*time.Millisecond)
	dir := runtimeTestDir(t)
	pid, reaped := startReapedTERMIgnoringProcess(t)
	createTime, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:               pid,
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:1",
		ProcessIdentityV2: "future-v1:12345",
		Metadata: map[string]string{
			runtimeCreateTime: strconv.FormatInt(createTime, 10),
		},
	})
	require.NoError(t, err)

	err = stopDaemonProcess(onlyLiveRuntimeRecord(t, dir), 50*time.Millisecond)
	require.Error(t, err)
	require.ErrorContains(t, err, "identity")
	assert.True(t, daemon.ProcessAlive(pid))
	assert.FileExists(t, path)
	select {
	case <-reaped:
		assert.Fail(t, "unknown v2 identity must not force-kill the process")
	default:
	}
}

func TestStartupFallbackStopUsesDaemonIdentity(t *testing.T) {
	dir := runtimeTestDir(t)
	pid := startSleepProcess(t)
	endpoint := newPingDaemonWithPID(t, pid)
	createTime, ok := processCreateTimeMillis(pid)
	require.True(t, ok)
	writeStartupFallbackFixture(
		t, dir, endpoint.Host, endpoint.Port, pid, strconv.FormatInt(createTime, 10),
	)

	rt := FindWritableDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.Equal(t, pid, rt.Record.PID)
	assert.Empty(t, rt.Record.ProcessIdentity)
	assert.Empty(t, rt.Record.ProcessIdentityV2)
	assert.Equal(t, processCreateTimeMatch, runtimeRecordIdentityState(rt.Record))
}
