package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/daemon"
)

func daemonRuntimeDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "runtime")
}

// freeTCPListener binds to a free loopback port and returns the
// listener (caller closes) and the port number. Tests that need
// an unreachable daemon close the listener after reserving the port.
func freeTCPListener(t *testing.T) (net.Listener, int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	port := l.Addr().(*net.TCPAddr).Port
	return l, port
}

// writeDaemonRuntimeForTest writes a runtime record for dir and
// registers its removal, centralizing the WriteDaemonRuntime plus
// RemoveDaemonRuntime cleanup pairing the transport tests repeat.
func writeDaemonRuntimeForTest(
	t *testing.T, dir, host string, port int, daemonVersion string, readOnly bool,
) {
	t.Helper()
	_, err := WriteDaemonRuntime(dir, host, port, daemonVersion, readOnly)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })
}

// writeUnreachableDaemonRuntime reserves a free loopback port, releases
// it so the TCP probe fails, then writes a live-PID runtime record for
// it and registers cleanup. The record looks owned (live PID) but the
// daemon cannot be reached. Returns the unreachable port.
func writeUnreachableDaemonRuntime(t *testing.T, dir string, readOnly bool) int {
	t.Helper()
	ln, port := freeTCPListener(t)
	ln.Close()
	writeDaemonRuntimeForTest(t, dir, "127.0.0.1", port, "test", readOnly)
	return port
}

func TestDetectTransport_UsesStartupStateFallbackWithoutRuntimeRecord(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10))

	tr, err := detectTransportContext(context.Background(), dir, "", time.Second)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, fmt.Sprintf("http://%s:%d", host, port), tr.URL)
}

// incompatibleRuntimeRecord builds a writable runtime record whose API
// version metadata is "0", which the compatibility check rejects. It
// models a daemon from an older release that still owns the archive.
func incompatibleRuntimeRecord(
	host string, port int, daemonVersion string, noSync bool,
) daemon.RuntimeRecord {
	meta := map[string]string{
		runtimeHost:        host,
		runtimePort:        strconv.Itoa(port),
		runtimeReadOnly:    "false",
		runtimeAPIVersion:  "0",
		runtimeDataVersion: strconv.Itoa(db.CurrentDataVersion()),
	}
	if noSync {
		meta[runtimeNoSync] = "true"
	}
	return daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   net.JoinHostPort(host, strconv.Itoa(port)),
		Service:   daemonService,
		Version:   daemonVersion,
		StartedAt: time.Now(),
		Metadata:  meta,
	}
}

// writeIncompatibleDaemonRuntime writes an incompatibleRuntimeRecord for
// dir and registers its removal.
func writeIncompatibleDaemonRuntime(
	t *testing.T, dir, host string, port int, daemonVersion string, noSync bool,
) {
	t.Helper()
	_, err := writeRuntimeRecordForTest(
		dir, incompatibleRuntimeRecord(host, port, daemonVersion, noSync),
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })
}

// setTestVersion overrides the package build version for the duration of
// the test and restores it on cleanup.
func setTestVersion(t *testing.T, value string) {
	t.Helper()
	old := version
	version = value
	t.Cleanup(func() { version = old })
}

// stubStartBackgroundServeForTransport swaps the background-serve hook
// for the test and restores the original on cleanup.
func stubStartBackgroundServeForTransport(
	t *testing.T,
	fn func(context.Context, *config.Config, time.Duration) (*DaemonRuntime, error),
) {
	t.Helper()
	old := startBackgroundServeForTransport
	startBackgroundServeForTransport = fn
	t.Cleanup(func() { startBackgroundServeForTransport = old })
}

// forbidStartBackgroundServeForTransport fails the test if the
// background-serve hook is invoked, with msg describing the violation.
func forbidStartBackgroundServeForTransport(t *testing.T, msg string) {
	t.Helper()
	stubStartBackgroundServeForTransport(t, func(
		context.Context, *config.Config, time.Duration,
	) (*DaemonRuntime, error) {
		t.Helper()
		t.Fatal(msg)
		return nil, nil
	})
}

// stubStopDaemonRuntimeForUpgrade swaps the upgrade-stop hook for the
// test and restores the original on cleanup.
func stubStopDaemonRuntimeForUpgrade(
	t *testing.T,
	fn func(config.Config, *DaemonRuntime) error,
) {
	t.Helper()
	old := stopDaemonRuntimeForUpgrade
	stopDaemonRuntimeForUpgrade = fn
	t.Cleanup(func() { stopDaemonRuntimeForUpgrade = old })
}

// forbidStopDaemonRuntimeForUpgrade fails the test if the upgrade-stop
// hook is invoked, with msg describing the violation.
func forbidStopDaemonRuntimeForUpgrade(t *testing.T, msg string) {
	t.Helper()
	stubStopDaemonRuntimeForUpgrade(t, func(
		config.Config, *DaemonRuntime,
	) error {
		t.Helper()
		t.Fatal(msg)
		return nil
	})
}

// stubWaitForDaemonStartupForTransport swaps the startup-wait hook for
// the test and restores the original on cleanup.
func stubWaitForDaemonStartupForTransport(
	t *testing.T,
	fn func(context.Context, string, time.Duration, ...string) bool,
) {
	t.Helper()
	old := waitForDaemonStartupForTransport
	waitForDaemonStartupForTransport = fn
	t.Cleanup(func() { waitForDaemonStartupForTransport = old })
}

func TestDetectTransport_NoDaemon_ReturnsDirect(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
	assert.False(t, tr.ReadOnly)
	assert.Empty(t, tr.URL)
}

func TestDetectTransport_LocalServe_ReturnsHTTPWriteCapable(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, host, port, "test", false)

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.False(t, tr.ReadOnly)
	assert.Contains(t, tr.URL, "http://127.0.0.1:"+strconv.Itoa(port))
}

func TestDetectTransport_PGServe_ReturnsReadOnlyHTTP(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, host, port, "test", true)

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.True(t, tr.ReadOnly)
	assert.Contains(t, tr.URL, "http://127.0.0.1:"+strconv.Itoa(port))
}

func TestDetectTransport_AuthenticatedDaemonUsesBearerToken(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	host, port := testAuthenticatedPingServer(t, "secret")
	writeDaemonRuntimeForTest(t, dir, host, port, "test", false)

	tr, err := detectTransport(dir, "secret", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.False(t, tr.ReadOnly)
	assert.Contains(t, tr.URL, "http://127.0.0.1:"+strconv.Itoa(port))
}

// TestDetectTransport_LocalServeWritableRecordWins verifies that a
// writable kit runtime record is exposed as a write-capable HTTP transport.
func TestDetectTransport_LocalServeWritableRecordWins(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	host, writablePort := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, host, writablePort, "test", false)

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.False(t, tr.ReadOnly)
	assert.Contains(t, tr.URL,
		"http://127.0.0.1:"+strconv.Itoa(writablePort),
		"expected URL to point at the writable daemon")
}

// TestDetectTransport_PGServeUnreachable_AllowsDirectWrite verifies
// that an unprobeable pg serve runtime record does not prove daemon
// ownership, so direct access remains available.
func TestDetectTransport_PGServeUnreachable_AllowsDirectWrite(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	writeUnreachableDaemonRuntime(t, dir, true) // readOnly = pg serve

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
}

// TestDetectTransport_LocalDaemonUnreachable_SetsDirectReadOnly verifies
// that a writable runtime record suppresses direct writes even when
// the daemon ping is temporarily unavailable.
func TestDetectTransport_LocalDaemonUnreachable_SetsDirectReadOnly(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	writeUnreachableDaemonRuntime(t, dir, false) // writable local

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
	assert.True(t, tr.DirectReadOnly)
}

func TestDetectTransport_IncompatibleDaemonSetsDirectReason(t *testing.T) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeIncompatibleDaemonRuntime(t, dir, host, port, "old", false)

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
	assert.True(t, tr.DirectReadOnly)
	assert.True(t, tr.DirectIncompatible)
	assert.Contains(t, tr.DirectReason, "API version")
}

func TestDetectTransport_LocalDaemonUnreachableDoesNotSetDirectIncompatible(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	writeUnreachableDaemonRuntime(t, dir, false)

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
	assert.True(t, tr.DirectReadOnly)
	assert.False(t, tr.DirectIncompatible)
	assert.Equal(t, errLocalDaemonUnreachable.Error(), tr.DirectReason)
}

// TestDetectTransport_DaemonStarting simulates a server that's
// starting up (start lock held, no runtime record, no listener).
// The held kit lock makes IsDaemonStarting return true.
// The helper waits out the timeout then falls back to direct.
func TestDetectTransport_DaemonStarting_FallsBackToDirect(t *testing.T) {
	t.Parallel()
	dir := daemonRuntimeDir(t)
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })

	tr, err := detectTransport(dir, "", 100*time.Millisecond)
	require.NoError(t, err)
	// Still no runtime record after wait, so IsDaemonActive sees
	// only the start lock and returns direct (writable) since
	// no runtime record means no daemon claim.
	assert.Equal(t, transportDirect, tr.Mode)
}

func TestEnsureTransport_ReadIntentStartsDaemon(t *testing.T) {
	dir := daemonRuntimeDir(t)
	cfg := config.Config{DataDir: dir}
	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, gotCfg *config.Config, wait time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.Equal(t, dir, gotCfg.DataDir)
		assert.Equal(t, backgroundAutoStartReadyTimeout, wait)
		return &DaemonRuntime{
			Host: "127.0.0.1",
			Port: 12345,
		}, nil
	})

	tr, err := ensureTransport(&cfg, transportIntentRead, 0)
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://127.0.0.1:12345", tr.URL)
}

func TestEnsureTransport_ReadIntentNoDaemonEnvRefusesDirectRead(t *testing.T) {
	dir := daemonRuntimeDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	cfg := config.Config{DataDir: dir}
	forbidStartBackgroundServeForTransport(t,
		"AGENTSVIEW_NO_DAEMON must suppress daemon start")

	_, err := ensureTransport(&cfg, transportIntentRead, 100*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "direct SQLite reads are not supported")
	assert.Contains(t, err.Error(), "agentsview daemon start")
}

func TestEnsureTransport_ReadIntentUnreachableDaemonRefusesDirectRead(t *testing.T) {
	dir := daemonRuntimeDir(t)
	writeUnreachableDaemonRuntime(t, dir, false)
	cfg := config.Config{DataDir: dir}
	forbidStartBackgroundServeForTransport(t,
		"unreachable live daemon must not trigger a second start")

	_, err := ensureTransport(&cfg, transportIntentRead, 100*time.Millisecond)
	require.ErrorIs(t, err, errLocalDaemonUnreachable)
}

func TestEnsureTransport_ArchiveWriteStartsDaemon(t *testing.T) {
	dir := daemonRuntimeDir(t)
	cfg := config.Config{DataDir: dir, AuthToken: "secret"}
	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, gotCfg *config.Config, wait time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.Equal(t, dir, gotCfg.DataDir)
		assert.Equal(t, 100*time.Millisecond, wait)
		return &DaemonRuntime{
			Host: "127.0.0.1",
			Port: 12345,
		}, nil
	})

	tr, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, 100*time.Millisecond,
	)
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://127.0.0.1:12345", tr.URL)
}

func TestEnsureTransport_ArchiveWriteRestartsOlderDaemon(t *testing.T) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dir, host, port, "1.0.0", false, false, true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	setTestVersion(t, "1.1.0")

	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, gotCfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.True(t, gotCfg.NoSync)
		return &DaemonRuntime{
			Host: "127.0.0.1",
			Port: 23456,
		}, nil
	})

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, 100*time.Millisecond,
	)
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://127.0.0.1:23456", tr.URL)
}

func TestEnsureTransport_ReadIntentRestartsOlderDaemon(t *testing.T) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dir, host, port, "1.0.0", false, false, true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	setTestVersion(t, "1.1.0")

	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, gotCfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.True(t, gotCfg.NoSync)
		return &DaemonRuntime{
			Host: "127.0.0.1",
			Port: 23456,
		}, nil
	})

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentRead, 100*time.Millisecond,
	)
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://127.0.0.1:23456", tr.URL)
}

func TestEnsureTransport_ReadIntentNoDaemonEnvRefusesOlderDaemon(
	t *testing.T,
) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, host, port, "1.0.0", false)

	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	setTestVersion(t, "1.1.0")
	forbidStartBackgroundServeForTransport(t,
		"AGENTSVIEW_NO_DAEMON must suppress daemon replacement")

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentRead, 100*time.Millisecond,
	)

	require.Error(t, err)
	assert.Equal(t, transport{}, tr)
	assert.Contains(t, err.Error(), "daemon restart required")
	assert.Contains(t, err.Error(), "agentsview daemon restart")
}

func TestEnsureTransport_ReadIntentPreservesExplicitNoSyncWhenRestartingOlderDaemon(
	t *testing.T,
) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dir, host, port, "1.0.0", false, false, false,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })

	setTestVersion(t, "1.1.0")

	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, gotCfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.True(t, gotCfg.NoSync)
		return &DaemonRuntime{
			Host: "127.0.0.1",
			Port: 23456,
		}, nil
	})

	cfg := config.Config{DataDir: dir, NoSync: true}
	tr, err := ensureTransport(
		&cfg, transportIntentRead, 100*time.Millisecond,
	)
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, transportHTTP, tr.Mode)
}

func TestEnsureTransport_ArchiveWriteNoDaemonEnvKeepsOlderDaemon(t *testing.T) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, host, port, "1.0.0", false)

	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	setTestVersion(t, "1.1.0")
	forbidStopDaemonRuntimeForUpgrade(t,
		"AGENTSVIEW_NO_DAEMON must not replace an older daemon")
	forbidStartBackgroundServeForTransport(t,
		"AGENTSVIEW_NO_DAEMON must not start a replacement daemon")

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, 100*time.Millisecond,
	)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://"+net.JoinHostPort(host, strconv.Itoa(port)), tr.URL)
}

func TestEnsureTransport_ArchiveWriteStopsOlderDaemonUnderLaunchLock(
	t *testing.T,
) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, host, port, "1.0.0", false)

	// This test exercises the real auto-start machinery; bypass the
	// test-binary guard in autoStartBackgroundServe.
	stubStartBackgroundServeForTransport(t, ensureBackgroundServe)

	setTestVersion(t, "1.1.0")
	stopErr := errors.New("stop after launch lock")
	stubStopDaemonRuntimeForUpgrade(t, func(
		config.Config, *DaemonRuntime,
	) error {
		launchLock, ok := acquireBackgroundLaunchLock(dir)
		if ok {
			require.NoError(t, launchLock.Unlock())
			assert.Fail(t, "older daemon stopped without launch lock held")
		}
		return stopErr
	})

	cfg := config.Config{DataDir: dir}
	_, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, 100*time.Millisecond,
	)
	assert.ErrorIs(t, err, stopErr)
}

func TestEnsureTransport_ArchiveWriteRestartsIncompatibleOlderDaemon(t *testing.T) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeIncompatibleDaemonRuntime(t, dir, host, port, "1.0.0", true)

	setTestVersion(t, "1.1.0")

	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, gotCfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.True(t, gotCfg.NoSync)
		return &DaemonRuntime{
			Host: "127.0.0.1",
			Port: 23456,
		}, nil
	})

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, 100*time.Millisecond,
	)
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://127.0.0.1:23456", tr.URL)
}

func TestEnsureTransport_ReadIntentPreservesExplicitNoSyncWhenRestartingIncompatibleDaemon(
	t *testing.T,
) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeIncompatibleDaemonRuntime(t, dir, host, port, "1.0.0", false)

	setTestVersion(t, "1.1.0")

	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, gotCfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.True(t, gotCfg.NoSync)
		return &DaemonRuntime{
			Host: "127.0.0.1",
			Port: 23456,
		}, nil
	})

	cfg := config.Config{DataDir: dir, NoSync: true}
	tr, err := ensureTransport(
		&cfg, transportIntentRead, 100*time.Millisecond,
	)
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, transportHTTP, tr.Mode)
}

func TestEnsureTransport_ReadIntentRestartsIncompatibleOlderDaemon(t *testing.T) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeIncompatibleDaemonRuntime(t, dir, host, port, "1.0.0", true)

	setTestVersion(t, "1.1.0")

	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, gotCfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.True(t, gotCfg.NoSync)
		return &DaemonRuntime{
			Host: "127.0.0.1",
			Port: 23456,
		}, nil
	})

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentRead, 100*time.Millisecond,
	)
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://127.0.0.1:23456", tr.URL)
}

func TestEnsureTransport_ArchiveWriteRestartsIncompatibleDaemonAfterExternalStartupAbort(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeIncompatibleDaemonRuntime(t, dir, host, port, "1.0.0", true)
	unlockStart := holdExternalDaemonStartLock(t, dir)

	setTestVersion(t, "1.1.0")

	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, gotCfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.Equal(t, dir, gotCfg.DataDir)
		assert.True(t, gotCfg.NoSync)
		return &DaemonRuntime{
			Host: "127.0.0.1",
			Port: 23456,
		}, nil
	})

	released := make(chan struct{})
	go func() {
		time.Sleep(2 * startProbeTick())
		unlockStart()
		close(released)
	}()

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, time.Second,
	)

	<-released
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://127.0.0.1:23456", tr.URL)
}

func TestEnsureTransport_ArchiveWriteRejectsUnsafeOlderDaemonRestart(t *testing.T) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, host, port, "1.0.0", false)

	setTestVersion(t, "1.1.0")
	forbidStopDaemonRuntimeForUpgrade(t,
		"unsafe restart must be rejected before stopping daemon")
	forbidStartBackgroundServeForTransport(t,
		"unsafe restart must be rejected before daemon start")

	cfg := config.Config{DataDir: dir, Host: "0.0.0.0"}
	_, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, 100*time.Millisecond,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to auto-start")
	assert.Contains(t, err.Error(), "0.0.0.0")
}

func TestEnsureTransport_ArchiveWriteDoesNotDowngradeNewerDaemon(t *testing.T) {
	dir := daemonRuntimeDir(t)
	host, port := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, host, port, "1.1.0", false)

	setTestVersion(t, "1.0.0")
	forbidStopDaemonRuntimeForUpgrade(t, "older CLI must not stop a newer daemon")
	forbidStartBackgroundServeForTransport(t,
		"older CLI must not start over a newer daemon")

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, 100*time.Millisecond,
	)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://"+net.JoinHostPort(host, strconv.Itoa(port)), tr.URL)
}

func TestShouldUpgradeDaemonRuntimeTreatsMissingDaemonVersionAsOlderRelease(t *testing.T) {
	rt := &DaemonRuntime{}

	assert.True(t, shouldUpgradeDaemonRuntime(rt, "1.1.0"))
	assert.False(t, shouldUpgradeDaemonRuntime(rt, "dev"))
}

func TestEnsureTransport_ArchiveWriteRefusesUnauthenticatedNonLoopbackAutoStart(
	t *testing.T,
) {
	dir := daemonRuntimeDir(t)
	cfg := config.Config{DataDir: dir, Host: "0.0.0.0"}
	forbidStartBackgroundServeForTransport(t,
		"unsafe auto-start must be rejected before launch")

	_, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, 100*time.Millisecond,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refusing to auto-start")
	assert.Contains(t, err.Error(), "0.0.0.0")
}

func TestEnsureTransport_ArchiveWriteAllowsAuthenticatedNonLoopbackAutoStart(
	t *testing.T,
) {
	dir := daemonRuntimeDir(t)
	cfg := config.Config{
		DataDir:     dir,
		Host:        "0.0.0.0",
		RequireAuth: true,
	}
	var started bool
	stubStartBackgroundServeForTransport(t, func(
		context.Context, *config.Config, time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		return &DaemonRuntime{Host: "0.0.0.0", Port: 12345}, nil
	})

	tr, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, 100*time.Millisecond,
	)
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, "http://127.0.0.1:12345", tr.URL)
}

func TestEnsureTransport_ArchiveWriteUsesAutoStartWaitForStartingDaemon(
	t *testing.T,
) {
	dir := daemonRuntimeDir(t)
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })

	var gotWait time.Duration
	stubWaitForDaemonStartupForTransport(t, func(
		_ context.Context, dataDir string, wait time.Duration, _ ...string,
	) bool {
		gotWait = wait
		UnmarkDaemonStarting(dataDir)
		return false
	})

	stubStartBackgroundServeForTransport(t, func(
		context.Context, *config.Config, time.Duration,
	) (*DaemonRuntime, error) {
		return &DaemonRuntime{Host: "127.0.0.1", Port: 12345}, nil
	})

	cfg := config.Config{DataDir: dir}
	_, err := ensureTransport(&cfg, transportIntentArchiveWrite, 0)
	require.NoError(t, err)
	assert.Equal(t, backgroundAutoStartReadyTimeout, gotWait)
}

func TestDetectTransportWaitsForExternalStartLockBeforeReturningRuntime(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := daemonRuntimeDir(t)
	oldHost, oldPort := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, oldHost, oldPort, "1.0.0", false)
	unlockStart := holdExternalDaemonStartLock(t, dir)

	newHost, newPort := testPingServer(t)
	published := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		RemoveDaemonRuntime(dir)
		_, err := WriteDaemonRuntime(dir, newHost, newPort, "1.1.0", false)
		unlockStart()
		published <- err
	}()

	tr, err := detectTransportContext(
		context.Background(), dir, "", time.Second,
	)

	require.NoError(t, <-published)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://"+net.JoinHostPort(
		newHost, strconv.Itoa(newPort),
	), tr.URL)
}

func TestEnsureTransportArchiveWriteWaitsForBackgroundReplacementLock(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := daemonRuntimeDir(t)
	oldHost, oldPort := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, oldHost, oldPort, version, false)
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	t.Cleanup(func() { _ = launchLock.Unlock() })

	newHost, newPort := testPingServer(t)
	published := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		RemoveDaemonRuntime(dir)
		_, err := WriteDaemonRuntime(dir, newHost, newPort, version, false)
		if err == nil {
			err = launchLock.Unlock()
		}
		published <- err
	}()

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, time.Second,
	)

	require.NoError(t, <-published)
	require.NoError(t, err)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://"+net.JoinHostPort(
		newHost, strconv.Itoa(newPort),
	), tr.URL)
}

func TestEnsureTransportArchiveWriteAdoptsAuthAfterBackgroundLaunchWait(
	t *testing.T,
) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := daemonRuntimeDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	oldHost, oldPort := testPingServer(t)
	writeDaemonRuntimeForTest(t, dir, oldHost, oldPort, version, false)
	launchLock, ok := acquireBackgroundLaunchLock(dir)
	require.True(t, ok)
	t.Cleanup(func() { _ = launchLock.Unlock() })

	const token = "generated-token"
	newHost, newPort := testAuthenticatedPingServer(t, token)
	published := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		writeTestConfig(t, dir, `require_auth = true
auth_token = "generated-token"
`)
		RemoveDaemonRuntime(dir)
		_, err := WriteDaemonRuntimeWithAuth(
			dir, newHost, newPort, version, false, true,
		)
		if err == nil {
			err = launchLock.Unlock()
		}
		published <- err
	}()

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentArchiveWrite, time.Second,
	)

	require.NoError(t, <-published)
	require.NoError(t, err)
	assert.Equal(t, token, cfg.AuthToken)
	assert.True(t, cfg.RequireAuth)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://"+net.JoinHostPort(
		newHost, strconv.Itoa(newPort),
	), tr.URL)
}

func TestEnsureTransportReadAdoptsAuthAfterDaemonStartupWait(t *testing.T) {
	setStartProbeTickForTest(t, 25*time.Millisecond)

	dir := daemonRuntimeDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	unlockStart := holdExternalDaemonStartLock(t, dir)

	const token = "generated-token"
	newHost, newPort := testAuthenticatedPingServer(t, token)
	published := make(chan error, 1)
	go func() {
		time.Sleep(2 * startProbeTick())
		writeTestConfig(t, dir, `require_auth = true
auth_token = "generated-token"
`)
		_, err := WriteDaemonRuntimeWithAuth(
			dir, newHost, newPort, version, false, true,
		)
		unlockStart()
		published <- err
	}()

	cfg := config.Config{DataDir: dir}
	tr, err := ensureTransport(
		&cfg, transportIntentRead, time.Second,
	)

	require.NoError(t, <-published)
	require.NoError(t, err)
	assert.Equal(t, token, cfg.AuthToken)
	assert.True(t, cfg.RequireAuth)
	assert.Equal(t, transportHTTP, tr.Mode)
	assert.Equal(t, "http://"+net.JoinHostPort(
		newHost, strconv.Itoa(newPort),
	), tr.URL)
}

func TestWaitForBackgroundLaunchBeforeArchiveWriteRejectsFileDataDir(
	t *testing.T,
) {
	dataDir := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(dataDir, []byte("not a dir"), 0o600))

	waited, err := waitForBackgroundLaunchBeforeArchiveWrite(
		context.Background(), dataDir, 10*time.Millisecond,
	)

	require.Error(t, err)
	assert.False(t, waited)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestEnsureTransportContextCancelDuringStartupWait(t *testing.T) {
	dir := daemonRuntimeDir(t)
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })

	ctx, cancel := context.WithCancel(context.Background())
	stubWaitForDaemonStartupForTransport(t, func(
		gotCtx context.Context,
		dataDir string,
		_ time.Duration,
		_ ...string,
	) bool {
		assert.Equal(t, dir, dataDir)
		cancel()
		<-gotCtx.Done()
		UnmarkDaemonStarting(dataDir)
		return false
	})

	forbidStartBackgroundServeForTransport(t,
		"canceled startup wait must not start a daemon")

	cfg := config.Config{DataDir: dir}
	_, err := ensureTransportContext(
		ctx, &cfg, transportIntentArchiveWrite, 100*time.Millisecond,
	)
	require.ErrorIs(t, err, context.Canceled)
}

func TestEnsureTransport_ArchiveWriteNoDaemonEnvUsesDirect(t *testing.T) {
	dir := daemonRuntimeDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	cfg := config.Config{DataDir: dir}
	forbidStartBackgroundServeForTransport(t,
		"AGENTSVIEW_NO_DAEMON must suppress daemon start")

	tr, err := ensureTransport(&cfg, transportIntentArchiveWrite, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, transportDirect, tr.Mode)
}

func TestEnsureTransport_ArchiveWriteUnreachableDaemonRefuses(t *testing.T) {
	dir := daemonRuntimeDir(t)
	writeUnreachableDaemonRuntime(t, dir, false)

	forbidStartBackgroundServeForTransport(t,
		"unreachable live daemon must not trigger a second start")

	cfg := config.Config{DataDir: dir}
	_, err := ensureTransport(&cfg, transportIntentArchiveWrite, 100*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, errLocalDaemonUnreachable)
}

func TestEnsureTransport_ArchiveWritePropagatesGeneratedAuthToken(t *testing.T) {
	dir := daemonRuntimeDir(t)
	cfg := config.Config{DataDir: dir}
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, gotCfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		gotCfg.AuthToken = "generated"
		return &DaemonRuntime{
			Host: "127.0.0.1",
			Port: 12345,
		}, nil
	})

	_, err := ensureTransport(&cfg, transportIntentArchiveWrite, 100*time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, "generated", cfg.AuthToken)
}

// TestNewService_HTTPMode verifies that newService returns a
// working HTTP-backed service and a cleanup function when the
// transport is HTTP mode. No DB is opened in this path.
func TestNewService_HTTPMode(t *testing.T) {
	t.Parallel()
	tr := transport{
		Mode: transportHTTP,
		URL:  "http://127.0.0.1:8080",
	}
	svc, cleanup, err := newService(config.Config{}, tr)
	require.NoError(t, err)
	require.NotNil(t, svc)
	require.NotNil(t, cleanup)
	cleanup()
}

// TestNewService_DirectMode verifies that newService opens the
// local SQLite DB and returns a direct-backed service when the
// transport is direct mode. The cleanup function must close the
// DB.
func TestNewService_DirectMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	seed, err := db.Open(dbPath)
	require.NoError(t, err)
	seed.Close()
	cfg := config.Config{DBPath: dbPath}

	svc, cleanup, err := newService(cfg, transport{Mode: transportDirect})
	require.NoError(t, err)
	require.NotNil(t, svc)
	require.NotNil(t, cleanup)
	cleanup()
}

// TestNewService_DirectReadOnly verifies that the DirectReadOnly branch
// opens the DB and returns a read-only service.
func TestNewService_DirectReadOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	seed, err := db.Open(dbPath)
	require.NoError(t, err)
	seed.Close()
	cfg := config.Config{DBPath: dbPath}

	svc, cleanup, err := newService(cfg, transport{
		Mode:           transportDirect,
		DirectReadOnly: true,
	})
	require.NoError(t, err)
	require.NotNil(t, svc)
	require.NotNil(t, cleanup)
	cleanup()
}

func TestNewService_DirectIncompatibleRefusesWithoutOpeningDB(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	cfg := config.Config{DBPath: dbPath}

	svc, cleanup, err := newService(cfg, transport{
		Mode:               transportDirect,
		DirectReadOnly:     true,
		DirectIncompatible: true,
		DirectReason:       "daemon data version 52 is incompatible with client data version 57",
	})

	require.Error(t, err)
	assert.Nil(t, svc)
	assert.Nil(t, cleanup)
	assert.Contains(t, err.Error(), "daemon data version 52 is incompatible")
	assert.Contains(t, err.Error(), "agentsview daemon restart")
	assert.NoFileExists(t, dbPath)
}

func TestNewService_DirectModeMissingDBDoesNotCreate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	cfg := config.Config{DBPath: dbPath}

	svc, cleanup, err := newService(cfg, transport{Mode: transportDirect})
	require.Error(t, err)
	assert.Nil(t, svc)
	assert.Nil(t, cleanup)
	assert.NoFileExists(t, dbPath)
}

func TestUrlFromDaemonRuntime_BindAllMapsToLoopback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		host string
		want string
	}{
		{"", "http://127.0.0.1:8080"},
		{"0.0.0.0", "http://127.0.0.1:8080"},
		{"::", "http://[::1]:8080"},
		{"192.168.1.10", "http://192.168.1.10:8080"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			got := urlFromDaemonRuntime(&DaemonRuntime{
				Host: tc.host,
				Port: 8080,
			})
			assert.Equal(t, tc.want, got)
		})
	}
}
