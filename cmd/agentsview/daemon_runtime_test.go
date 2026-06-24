package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/daemon"
)

// requirePOSIXSignals skips the test on platforms without POSIX signal
// semantics, where graceful SIGTERM termination and zombie-based liveness
// checks do not apply.
func requirePOSIXSignals(t *testing.T, reason string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(reason)
	}
}

// startSleepProcess starts a long-lived child process and reaps it during
// cleanup. The returned PID is alive for the duration of the test, and once
// signalled the child becomes a zombie that daemon.ProcessAlive still reports
// as alive until cleanup reaps it.
func startSleepProcess(t *testing.T) int {
	t.Helper()
	return startProcessKilledOnCleanup(t, exec.Command("sleep", "60"))
}

// startTERMIgnoringProcess starts a child that ignores SIGTERM, so it survives
// a graceful stop and drives the force-kill escalation path.
func startTERMIgnoringProcess(t *testing.T) int {
	t.Helper()
	return startProcessKilledOnCleanup(
		t, exec.Command("sh", "-c", "trap '' TERM; sleep 60"),
	)
}

func startProcessKilledOnCleanup(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// startReapedSleepProcess starts a long-lived child and reaps it concurrently,
// so once the process is signalled it leaves the zombie state and
// daemon.ProcessAlive reports it as dead. The returned channel closes when the
// child has been reaped.
func startReapedSleepProcess(t *testing.T) (int, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd.Process.Pid, reaped
}

// startReapedProcess starts and fully reaps a short-lived process, returning a
// PID that is guaranteed dead.
func startReapedProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit")
	}
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())
	return pid
}

// onlyLiveRuntimeRecord asserts exactly one live runtime record exists in dir
// and returns it.
func onlyLiveRuntimeRecord(t *testing.T, dir string) daemon.RuntimeRecord {
	t.Helper()
	records := liveDaemonRecords(dir)
	require.Len(t, records, 1)
	return records[0]
}

func writeRuntimeRecordForTest(
	dataDir string, rec daemon.RuntimeRecord,
) (string, error) {
	if rec.Service == "" {
		rec.Service = daemonService
	}
	if rec.Metadata == nil {
		rec.Metadata = map[string]string{}
	}
	return runtimeStore(dataDir).Write(rec)
}

func runtimePathForTest(dataDir string, pid int) string {
	path, err := runtimeStore(dataDir).Path(pid)
	if err != nil {
		return filepath.Join(dataDir, fmt.Sprintf("daemon.%d.json", pid))
	}
	return path
}

func runtimeTestDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runtime")
	_, err := runtimeStore(dir).LockPath()
	require.NoError(t, err)
	return dir
}

func testPingServer(t *testing.T) (host string, port int) {
	t.Helper()
	ts := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	}))
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err = strconv.Atoi(portText)
	require.NoError(t, err)
	return host, port
}

func testAuthenticatedPingServer(
	t *testing.T, token string,
) (host string, port int) {
	t.Helper()
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ping.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err = strconv.Atoi(portText)
	require.NoError(t, err)
	return host, port
}

func writeLegacyRuntimeStateForTest(
	t *testing.T,
	dataDir string,
	state legacyStateFile,
) string {
	t.Helper()
	if state.Port <= 0 {
		state.Port = 59999
	}
	path := filepath.Join(dataDir, fmt.Sprintf("server.%d.json", state.Port))
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}

func TestWriteAndRemoveDaemonRuntime(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)

	path, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dir, host, port, "1.0.0", false, true, true,
	)
	require.NoError(t, err)
	assert.Equal(t, runtimePathForTest(dir, os.Getpid()), path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var rec daemon.RuntimeRecord
	require.NoError(t, json.Unmarshal(data, &rec))
	assert.Equal(t, daemonService, rec.Service)
	assert.Equal(t, "1.0.0", rec.Version)
	assert.Equal(t, os.Getpid(), rec.PID)
	assert.Equal(t, daemon.NetworkTCP, rec.Network)
	assert.Equal(t, net.JoinHostPort(host, strconv.Itoa(port)), rec.Address)
	assert.Equal(t, "true", rec.Metadata[runtimeRequireAuth])
	assert.Equal(t, "true", rec.Metadata[runtimeNoSync])
	assert.Equal(t, strconv.Itoa(daemonAPIVersion), rec.Metadata[runtimeAPIVersion])
	assert.Equal(t, strconv.Itoa(db.CurrentDataVersion()), rec.Metadata[runtimeDataVersion])

	rt := daemonRuntimeFromRecord(rec)
	assert.True(t, rt.RequireAuth)
	assert.True(t, rt.RequireAuthKnown)
	assert.True(t, rt.NoSync)

	RemoveDaemonRuntime(dir)
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "runtime record not removed")
}

func TestFindDaemonRuntime_NoFiles(t *testing.T) {
	dir := runtimeTestDir(t)
	assert.Nil(t, FindDaemonRuntime(dir))
}

func TestFindDaemonRuntime_StaleFile(t *testing.T) {
	dir := runtimeTestDir(t)
	deadPID := 999999999
	if daemon.ProcessAlive(deadPID) {
		t.Skipf("pid %d is alive on this host", deadPID)
	}
	_, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:       deadPID,
		Network:   daemon.NetworkTCP,
		Address:   "127.0.0.1:9999",
		Service:   daemonService,
		Version:   "1.0.0",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)

	assert.Nil(t, FindDaemonRuntime(dir), "expected nil for stale PID")
	assert.False(t, IsDaemonActive(dir), "dead runtime record should not be active")
}

func TestFindDaemonRuntime_InvalidJSON(t *testing.T) {
	dir := runtimeTestDir(t)
	path := runtimePathForTest(dir, os.Getpid())
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o644))

	assert.Nil(t, FindDaemonRuntime(dir), "expected nil for invalid JSON")
}

func TestFindDaemonRuntime_IgnoresNonRuntimeFiles(t *testing.T) {
	dir := runtimeTestDir(t)
	require.NoError(t, os.WriteFile(
		runtimePathForTest(dir, os.Getpid())+".tmp",
		[]byte("{}"), 0o644,
	))
	require.NoError(t, os.WriteFile(
		dir+"/config.json",
		[]byte("{}"), 0o644,
	))

	assert.Nil(t, FindDaemonRuntime(dir))
}

func TestFindDaemonRuntime_LiveProcess(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(t, err)

	result := FindDaemonRuntime(dir)
	require.NotNil(t, result, "expected running server")
	assert.Equal(t, port, result.Port)
	assert.Equal(t, os.Getpid(), result.Record.PID)
	assert.False(t, result.ReadOnly)
}

func TestFindDaemonRuntime_IgnoresIncompatibleRuntime(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	_, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   net.JoinHostPort(host, strconv.Itoa(port)),
		Service:   daemonService,
		Version:   "old",
		StartedAt: time.Now(),
		Metadata: map[string]string{
			runtimeHost:        host,
			runtimePort:        strconv.Itoa(port),
			runtimeReadOnly:    "false",
			runtimeAPIVersion:  "0",
			runtimeDataVersion: strconv.Itoa(db.CurrentDataVersion()),
		},
	})
	require.NoError(t, err)

	assert.Nil(t, FindDaemonRuntime(dir))
	rt, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, rt)
	require.Error(t, compatErr)
	assert.Contains(t, compatErr.Error(), "API version")
	assert.True(t, IsLocalDaemonActive(dir),
		"incompatible writable daemon still owns the local archive")
}

func TestFindDaemonRuntime_ReadOnly(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", true)
	require.NoError(t, err)

	result := FindDaemonRuntime(dir)
	require.NotNil(t, result, "expected running server")
	assert.True(t, result.ReadOnly)
	assert.False(t, IsLocalDaemonActive(dir))
	assert.True(t, IsDaemonActive(dir))
}

func TestFindDaemonRuntime_BindAllMetadata(t *testing.T) {
	dir := runtimeTestDir(t)
	_, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, "0.0.0.0", port, "1.0.0", false)
	require.NoError(t, err)

	result := FindDaemonRuntime(dir)
	require.NotNil(t, result, "expected running server for bind-all host")
	assert.Equal(t, "0.0.0.0", result.Host)
	assert.Equal(t, port, result.Port)
}

func TestFindDaemonRuntime_UsesAuthToken(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testAuthenticatedPingServer(t, "secret")
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(t, err)

	assert.Nil(t, FindDaemonRuntime(dir),
		"expected no discovery without bearer token")

	result := FindDaemonRuntime(dir, "secret")
	require.NotNil(t, result, "expected discovery with bearer token")
	assert.Equal(t, port, result.Port)
	assert.False(t, result.ReadOnly)
}

func TestIsDaemonActive_LivePIDNoPingClaimsOwnership(t *testing.T) {
	dir := runtimeTestDir(t)
	_, err := WriteDaemonRuntime(dir, "127.0.0.1", 59999, "1.0.0", false)
	require.NoError(t, err)

	assert.Nil(t, FindDaemonRuntime(dir), "expected no discoverable daemon")
	assert.True(t, IsDaemonActive(dir),
		"live runtime record should still suppress writes")
	assert.True(t, IsLocalDaemonActive(dir),
		"live writable runtime record should claim the SQLite archive")
}

func TestIsDaemonActive_DeadPIDDaemonRuntime(t *testing.T) {
	dir := runtimeTestDir(t)
	deadPID := 999999999
	if daemon.ProcessAlive(deadPID) {
		t.Skipf("pid %d is alive on this host", deadPID)
	}
	path, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:       deadPID,
		Network:   daemon.NetworkTCP,
		Address:   "127.0.0.1:59994",
		Service:   daemonService,
		StartedAt: time.Now(),
	})
	require.NoError(t, err)

	assert.False(t, IsDaemonActive(dir), "expected false for dead PID runtime record")
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "dead runtime record not cleaned up")
}

func TestIsDaemonActive_StartLock(t *testing.T) {
	dir := runtimeTestDir(t)

	require.False(t, IsDaemonActive(dir), "expected false with no files")

	MarkDaemonStarting(dir)
	require.True(t, IsDaemonActive(dir), "expected true with start lock")

	UnmarkDaemonStarting(dir)
	require.False(t, IsDaemonActive(dir), "expected false after start lock released")
}

func TestFindDaemonRuntime_MigratesLegacyWritableStateFile(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	startedAt := time.Now().UTC().Add(-time.Minute)
	legacyPath := writeLegacyRuntimeStateForTest(t, dir, legacyStateFile{
		PID:       os.Getpid(),
		Host:      host,
		Port:      port,
		Version:   "legacy",
		StartedAt: startedAt.Format(time.RFC3339Nano),
	})

	result := FindDaemonRuntime(dir)
	require.Nil(t, result, "legacy state without compatibility metadata is incompatible")
	incompatible, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, incompatible, "expected legacy state to migrate")
	require.Error(t, compatErr)
	assert.Equal(t, os.Getpid(), incompatible.Record.PID)
	assert.Equal(t, host, incompatible.Host)
	assert.Equal(t, port, incompatible.Port)
	assert.Equal(t, "legacy", incompatible.Record.Version)
	assert.False(t, incompatible.ReadOnly)
	assert.True(t, IsDaemonActive(dir))
	assert.True(t, IsLocalDaemonActive(dir))

	_, legacyStatErr := os.Stat(legacyPath)
	assert.True(t, os.IsNotExist(legacyStatErr),
		"migrated legacy state file should be removed")
	runtimePath := runtimePathForTest(dir, os.Getpid())
	_, runtimeStatErr := os.Stat(runtimePath)
	assert.NoError(t, runtimeStatErr, "kit runtime record should be written")
}

func TestFindDaemonRuntime_MigratesLegacyReadOnlyStateFile(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	legacyPath := writeLegacyRuntimeStateForTest(t, dir, legacyStateFile{
		PID:      os.Getpid(),
		Host:     host,
		Port:     port,
		ReadOnly: true,
	})

	result := FindDaemonRuntime(dir)
	require.Nil(t, result, "read-only legacy state without compatibility metadata is incompatible")
	incompatible, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, incompatible, "expected read-only legacy state to migrate")
	require.Error(t, compatErr)
	assert.True(t, incompatible.ReadOnly)
	assert.False(t, IsLocalDaemonActive(dir))
	assert.True(t, IsDaemonActive(dir))
	_, legacyStatErr := os.Stat(legacyPath)
	assert.True(t, os.IsNotExist(legacyStatErr),
		"migrated legacy state file should be removed")
}

func TestFindDaemonRuntime_LegacyStateFileUsesPortFromName(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	legacyPath := writeLegacyRuntimeStateForTest(t, dir, legacyStateFile{
		PID:  os.Getpid(),
		Host: host,
		Port: port,
	})

	data, err := os.ReadFile(legacyPath)
	require.NoError(t, err)
	var state legacyStateFile
	require.NoError(t, json.Unmarshal(data, &state))
	state.Port = 0
	data, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(legacyPath, data, 0o644))

	result, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, result, "expected legacy port from file name to migrate")
	require.Error(t, compatErr)
	assert.Equal(t, port, result.Port)
}

func TestIsLocalDaemonActive_LegacyDeadPIDStateFileRemoved(t *testing.T) {
	dir := runtimeTestDir(t)
	deadPID := 999999999
	if daemon.ProcessAlive(deadPID) {
		t.Skipf("pid %d is alive on this host", deadPID)
	}
	path := writeLegacyRuntimeStateForTest(t, dir, legacyStateFile{PID: deadPID})

	assert.False(t, IsLocalDaemonActive(dir))
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "dead legacy state file not removed")
	_, runtimeStatErr := os.Stat(runtimePathForTest(dir, deadPID))
	assert.True(t, os.IsNotExist(runtimeStatErr),
		"dead legacy state should not become a kit runtime record")
}

func TestIsLocalDaemonActive_UnprobeableLegacyStateFileDoesNotSuppressWrites(
	t *testing.T,
) {
	dir := runtimeTestDir(t)
	path := writeLegacyRuntimeStateForTest(t, dir, legacyStateFile{
		PID:  os.Getpid(),
		Host: "127.0.0.1",
		Port: 59999,
	})

	assert.Nil(t, FindDaemonRuntime(dir), "unprobeable legacy state must not migrate")
	assert.False(t, IsDaemonActive(dir))
	assert.False(t, IsLocalDaemonActive(dir))
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "unprobeable live legacy state should be left intact")
	_, runtimeStatErr := os.Stat(runtimePathForTest(dir, os.Getpid()))
	assert.True(t, os.IsNotExist(runtimeStatErr),
		"unprobeable legacy state should not become a kit runtime record")
}

func TestIsDaemonStarting_LegacyStartupLock(t *testing.T) {
	dir := runtimeTestDir(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, fmt.Sprintf("%s%d", legacyStartupLockPrefix, os.Getpid())),
		[]byte(strconv.Itoa(os.Getpid())),
		0o644,
	))

	assert.True(t, IsDaemonStarting(dir))
	assert.True(t, IsLocalDaemonActive(dir))
}

func TestLiveWritableRuntimeWithMismatchedCreateTimeIsRemoved(t *testing.T) {
	dir := runtimeTestDir(t)
	liveCreateTime, ok := processCreateTimeMillis(os.Getpid())
	if !ok {
		t.Skip("process create time is unavailable on this platform")
	}
	_, err := writeRuntimeRecordForTest(dir, daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort("127.0.0.1", "9"),
		Metadata: map[string]string{
			runtimeReadOnly:   "false",
			runtimeCreateTime: strconv.FormatInt(liveCreateTime+1, 10),
		},
	})
	require.NoError(t, err)

	assert.False(t, hasLiveWritableDaemonRuntime(dir))
	assert.False(t, IsLocalDaemonActive(dir))
	_, statErr := os.Stat(runtimePathForTest(dir, os.Getpid()))
	assert.True(t, os.IsNotExist(statErr),
		"mismatched create-time runtime record should be removed")
}

func TestStartLock_OwnProcess(t *testing.T) {
	dir := runtimeTestDir(t)

	require.False(t, isDaemonStarting(dir), "expected false before lock written")

	MarkDaemonStarting(dir)
	require.True(t, isDaemonStarting(dir), "expected true after lock written")

	UnmarkDaemonStarting(dir)
	require.False(t, isDaemonStarting(dir), "expected false after start lock released")
}

func TestWaitForDaemonStartup_AlreadyRunning(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntime(dir, host, port, "1.0.0", false)
	require.NoError(t, err)

	assert.True(t, WaitForDaemonStartup(dir, 100*time.Millisecond),
		"expected true, server is running")
}

func TestWaitForDaemonStartup_LockClearsNoServer(t *testing.T) {
	dir := runtimeTestDir(t)

	assert.False(t, WaitForDaemonStartup(dir, 100*time.Millisecond),
		"expected false, no start lock and no server")
}

func TestProbeHostForDial(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"", "127.0.0.1"},
		{"0.0.0.0", "127.0.0.1"},
		{"::", "::1"},
		{"127.0.0.1", "127.0.0.1"},
		{"192.168.1.100", "192.168.1.100"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, probeHostForDial(tt.host),
			"probeHostForDial(%q)", tt.host)
	}
}

func TestDaemonRuntime_ReadOnlyPersisted(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)

	path, err := WriteDaemonRuntime(dir, host, port, "test", true)
	require.NoError(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var rec daemon.RuntimeRecord
	require.NoError(t, json.Unmarshal(raw, &rec))
	assert.Equal(t, "true", rec.Metadata[runtimeReadOnly])
	assert.Equal(t, strconv.Itoa(port), rec.Metadata[runtimePort])
	assert.Equal(t, "test", rec.Version)
}
