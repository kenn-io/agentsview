package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
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

func setStartProbeTickForTest(t *testing.T, tick time.Duration) {
	t.Helper()
	require.Positive(t, tick)
	old := time.Duration(atomic.SwapInt64(&startProbeTickNanos, int64(tick)))
	t.Cleanup(func() {
		atomic.StoreInt64(&startProbeTickNanos, int64(old))
	})
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

// deadPID returns a PID that daemon.ProcessAlive reports as dead. Reaping a
// just-started process is not portable here: on Windows, OpenProcess can still
// succeed briefly for a terminated process object.
func deadPID(t *testing.T) int {
	t.Helper()
	for _, pid := range []int{99999999, 999999999, 1 << 30} {
		if !daemon.ProcessAlive(pid) {
			return pid
		}
	}
	t.Skip("could not find an unused PID for stale runtime test")
	return 0
}

// onlyLiveRuntimeRecord asserts exactly one live runtime record exists in dir
// and returns it.
func onlyLiveRuntimeRecord(t *testing.T, dir string) daemon.RuntimeRecord {
	t.Helper()
	records := liveDaemonRecords(dir)
	require.Len(t, records, 1)
	return records[0]
}

type testDaemonEndpoint struct {
	Host string
	Port int
	Addr string
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

// writeRuntimeRecordFixture writes rec to dir, failing the test on error and
// registering cleanup that removes the runtime record.
func writeRuntimeRecordFixture(
	t *testing.T, dir string, rec daemon.RuntimeRecord,
) string {
	t.Helper()
	path, err := writeRuntimeRecordForTest(dir, rec)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dir) })
	return path
}

// runtimeRecordOption mutates a daemon.RuntimeRecord built by
// daemonRuntimeRecord.
type runtimeRecordOption func(*daemon.RuntimeRecord)

func withRuntimeVersion(v string) runtimeRecordOption {
	return func(rec *daemon.RuntimeRecord) { rec.Version = v }
}

func withRuntimePID(pid int) runtimeRecordOption {
	return func(rec *daemon.RuntimeRecord) { rec.PID = pid }
}

func withRuntimeAPIVersion(v int) runtimeRecordOption {
	return func(rec *daemon.RuntimeRecord) {
		rec.Metadata[runtimeAPIVersion] = strconv.Itoa(v)
	}
}

func withRuntimeNoSync(noSync bool) runtimeRecordOption {
	return func(rec *daemon.RuntimeRecord) {
		rec.Metadata[runtimeNoSync] = strconv.FormatBool(noSync)
	}
}

func withRuntimeReadOnly(readOnly bool) runtimeRecordOption {
	return func(rec *daemon.RuntimeRecord) {
		rec.Metadata[runtimeReadOnly] = strconv.FormatBool(readOnly)
	}
}

func withRuntimeRequireAuth(requireAuth bool) runtimeRecordOption {
	return func(rec *daemon.RuntimeRecord) {
		rec.Metadata[runtimeRequireAuth] = strconv.FormatBool(requireAuth)
	}
}

func withRuntimeMetadata(key, value string) runtimeRecordOption {
	return func(rec *daemon.RuntimeRecord) {
		rec.Metadata[key] = value
	}
}

// daemonRuntimeRecord builds a runtime record for the given address with the
// metadata fields the daemon writes. It defaults to a live, current-API,
// writable record; options override individual fields.
func daemonRuntimeRecord(
	host string, port int, opts ...runtimeRecordOption,
) daemon.RuntimeRecord {
	rec := daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   net.JoinHostPort(host, strconv.Itoa(port)),
		Service:   daemonService,
		Version:   "test",
		StartedAt: time.Now(),
		Metadata: map[string]string{
			runtimeHost:        host,
			runtimePort:        strconv.Itoa(port),
			runtimeReadOnly:    "false",
			runtimeAPIVersion:  strconv.Itoa(daemonAPIVersion),
			runtimeDataVersion: strconv.Itoa(db.CurrentDataVersion()),
		},
	}
	for _, opt := range opts {
		opt(&rec)
	}
	return rec
}

func runtimeRecordForEndpoint(
	endpoint testDaemonEndpoint, opts ...runtimeRecordOption,
) daemon.RuntimeRecord {
	return daemonRuntimeRecord(endpoint.Host, endpoint.Port, opts...)
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

func holdExternalDaemonStartLock(t *testing.T, dataDir string) func() {
	t.Helper()
	stdin := startExternalDaemonStartLockHelper(t, dataDir)

	var once sync.Once
	unlock := func() {
		once.Do(func() { _ = stdin.Close() })
	}
	t.Cleanup(unlock)
	return unlock
}

func startExternalDaemonStartLockHelper(t *testing.T, dataDir string) io.Closer {
	t.Helper()
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestHoldExternalDaemonStartLockHelperProcess$",
	)
	cmd.Env = append(
		os.Environ(),
		"AGENTSVIEW_HOLD_EXTERNAL_START_LOCK_HELPER=1",
		"AGENTSVIEW_HOLD_EXTERNAL_START_LOCK_DATA_DIR="+dataDir,
	)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		require.NoError(t, err)
	}
	if strings.TrimSpace(line) != "ready" {
		_ = cmd.Process.Kill()
		require.Equal(t, "ready", strings.TrimSpace(line))
	}
	require.Eventually(t, func() bool {
		return isExternalDaemonStarting(dataDir)
	}, 2*time.Second, 10*time.Millisecond,
		"external daemon start lock should be visible to parent")
	return stdin
}

func holdExternalBackgroundLaunchLock(t *testing.T, dataDir string) func() {
	t.Helper()
	stdin := startExternalBackgroundLaunchLockHelper(t, dataDir)

	var once sync.Once
	unlock := func() {
		once.Do(func() { _ = stdin.Close() })
	}
	t.Cleanup(unlock)
	return unlock
}

func startExternalBackgroundLaunchLockHelper(
	t *testing.T,
	dataDir string,
) io.Closer {
	t.Helper()
	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestHoldExternalBackgroundLaunchLockHelperProcess$",
	)
	cmd.Env = append(
		os.Environ(),
		"AGENTSVIEW_HOLD_EXTERNAL_BACKGROUND_LAUNCH_LOCK_HELPER=1",
		"AGENTSVIEW_HOLD_EXTERNAL_BACKGROUND_LAUNCH_LOCK_DATA_DIR="+dataDir,
	)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		require.NoError(t, err)
	}
	if strings.TrimSpace(line) != "ready" {
		_ = cmd.Process.Kill()
		require.Equal(t, "ready", strings.TrimSpace(line))
	}
	require.Eventually(t, func() bool {
		return isBackgroundLaunchActive(dataDir)
	}, 2*time.Second, 10*time.Millisecond,
		"external background launch lock should be visible to parent")
	return stdin
}

func TestHoldExternalDaemonStartLockHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_HOLD_EXTERNAL_START_LOCK_HELPER") != "1" {
		return
	}
	dataDir := os.Getenv("AGENTSVIEW_HOLD_EXTERNAL_START_LOCK_DATA_DIR")
	require.NotEmpty(t, dataDir)
	lockPath, err := runtimeStore(dataDir).LockPath()
	require.NoError(t, err)
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	require.NoError(t, lock.Unlock())
}

func TestHoldExternalBackgroundLaunchLockHelperProcess(t *testing.T) {
	if os.Getenv(
		"AGENTSVIEW_HOLD_EXTERNAL_BACKGROUND_LAUNCH_LOCK_HELPER",
	) != "1" {
		return
	}
	dataDir := os.Getenv(
		"AGENTSVIEW_HOLD_EXTERNAL_BACKGROUND_LAUNCH_LOCK_DATA_DIR",
	)
	require.NotEmpty(t, dataDir)
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	lock := flock.New(backgroundLaunchLockPath(dataDir))
	locked, err := lock.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	require.NoError(t, lock.Unlock())
}

func serverEndpoint(t *testing.T, ts *httptest.Server) testDaemonEndpoint {
	t.Helper()
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	return testDaemonEndpoint{
		Host: host,
		Port: port,
		Addr: net.JoinHostPort(host, portText),
	}
}

func newPingDaemon(t *testing.T) testDaemonEndpoint {
	t.Helper()
	ts := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	}))
	t.Cleanup(ts.Close)
	return serverEndpoint(t, ts)
}

func testPingServer(t *testing.T) (host string, port int) {
	t.Helper()
	endpoint := newPingDaemon(t)
	return endpoint.Host, endpoint.Port
}

func newAuthenticatedPingDaemon(t *testing.T, token string) testDaemonEndpoint {
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
	return serverEndpoint(t, ts)
}

func testAuthenticatedPingServer(
	t *testing.T, token string,
) (host string, port int) {
	t.Helper()
	endpoint := newAuthenticatedPingDaemon(t, token)
	return endpoint.Host, endpoint.Port
}

func writeLiveRuntime(
	t *testing.T,
	dir string,
	readOnly bool,
	opts ...runtimeRecordOption,
) (testDaemonEndpoint, string) {
	t.Helper()
	endpoint := newPingDaemon(t)
	allOpts := append([]runtimeRecordOption{withRuntimeReadOnly(readOnly)}, opts...)
	path := writeRuntimeRecordFixture(
		t, dir, runtimeRecordForEndpoint(endpoint, allOpts...),
	)
	return endpoint, path
}

func readRuntimeRecord(t *testing.T, path string) daemon.RuntimeRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var rec daemon.RuntimeRecord
	require.NoError(t, json.Unmarshal(data, &rec))
	return rec
}

func assertPathRemoved(t *testing.T, path string, msgAndArgs ...any) {
	t.Helper()
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), msgAndArgs...)
}

func assertRuntimeRecordRemoved(
	t *testing.T,
	dataDir string,
	pid int,
	msgAndArgs ...any,
) {
	t.Helper()
	if len(msgAndArgs) == 0 {
		msgAndArgs = []any{"runtime record should be removed"}
	}
	assertPathRemoved(t, runtimePathForTest(dataDir, pid), msgAndArgs...)
}

func requireFoundRuntime(
	t *testing.T, dataDir string, authTokens ...string,
) *DaemonRuntime {
	t.Helper()
	rt := FindDaemonRuntime(dataDir, authTokens...)
	require.NotNil(t, rt, "expected running server")
	return rt
}

func requireMigratedIncompatibleRuntime(
	t *testing.T, dataDir string,
) *DaemonRuntime {
	t.Helper()
	rt, err := FindIncompatibleDaemonRuntime(dataDir)
	require.NotNil(t, rt, "expected incompatible runtime")
	require.Error(t, err)
	return rt
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

func writeProbeableLegacyRuntime(
	t *testing.T,
	dataDir string,
	state legacyStateFile,
) (testDaemonEndpoint, string) {
	t.Helper()
	endpoint := newPingDaemon(t)
	if state.PID == 0 {
		state.PID = os.Getpid()
	}
	if state.Host == "" {
		state.Host = endpoint.Host
	}
	if state.Port == 0 {
		state.Port = endpoint.Port
	}
	return endpoint, writeLegacyRuntimeStateForTest(t, dataDir, state)
}

func rewriteLegacyState(
	t *testing.T,
	path string,
	mutate func(*legacyStateFile),
) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var state legacyStateFile
	require.NoError(t, json.Unmarshal(data, &state))
	mutate(&state)
	data, err = json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

func TestWriteAndRemoveDaemonRuntime(t *testing.T) {
	dir := runtimeTestDir(t)
	endpoint := newPingDaemon(t)

	path, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dir, endpoint.Host, endpoint.Port, "1.0.0", false, true, true,
	)
	require.NoError(t, err)
	assert.Equal(t, runtimePathForTest(dir, os.Getpid()), path)

	rec := readRuntimeRecord(t, path)
	assert.Equal(t, daemonService, rec.Service)
	assert.Equal(t, "1.0.0", rec.Version)
	assert.Equal(t, os.Getpid(), rec.PID)
	assert.Equal(t, daemon.NetworkTCP, rec.Network)
	assert.Equal(t, endpoint.Addr, rec.Address)
	assert.Equal(t, "true", rec.Metadata[runtimeRequireAuth])
	assert.Equal(t, "true", rec.Metadata[runtimeNoSync])
	assert.Equal(t, strconv.Itoa(daemonAPIVersion), rec.Metadata[runtimeAPIVersion])
	assert.Equal(t, strconv.Itoa(db.CurrentDataVersion()), rec.Metadata[runtimeDataVersion])

	rt := daemonRuntimeFromRecord(rec)
	assert.True(t, rt.RequireAuth)
	assert.True(t, rt.RequireAuthKnown)
	assert.True(t, rt.NoSync)

	RemoveDaemonRuntime(dir)
	assertPathRemoved(t, path, "runtime record not removed")
}

func TestFindDaemonRuntime_NoFiles(t *testing.T) {
	dir := runtimeTestDir(t)
	assert.Nil(t, FindDaemonRuntime(dir))
}

func TestFindDaemonRuntime_StaleFile(t *testing.T) {
	dir := runtimeTestDir(t)
	_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		"127.0.0.1", 9999,
		withRuntimePID(deadPID(t)),
		withRuntimeVersion("1.0.0"),
	))
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
	endpoint, _ := writeLiveRuntime(t, dir, false, withRuntimeVersion("1.0.0"))

	result := requireFoundRuntime(t, dir)
	assert.Equal(t, endpoint.Port, result.Port)
	assert.Equal(t, os.Getpid(), result.Record.PID)
	assert.False(t, result.ReadOnly)
}

func TestFindDaemonRuntime_IgnoresIncompatibleRuntime(t *testing.T) {
	dir := runtimeTestDir(t)
	writeLiveRuntime(t, dir, false,
		withRuntimeVersion("old"),
		withRuntimeAPIVersion(0),
	)

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
	writeLiveRuntime(t, dir, true, withRuntimeVersion("1.0.0"))

	result := requireFoundRuntime(t, dir)
	assert.True(t, result.ReadOnly)
	assert.False(t, IsLocalDaemonActive(dir))
	assert.True(t, IsDaemonActive(dir))
}

func TestFindDaemonRuntime_BindAllMetadata(t *testing.T) {
	dir := runtimeTestDir(t)
	endpoint := newPingDaemon(t)
	_, err := WriteDaemonRuntime(dir, "0.0.0.0", endpoint.Port, "1.0.0", false)
	require.NoError(t, err)

	result := requireFoundRuntime(t, dir)
	assert.Equal(t, "0.0.0.0", result.Host)
	assert.Equal(t, endpoint.Port, result.Port)
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
	writeRuntimeRecordFixture(t, dir, daemonRuntimeRecord(
		"127.0.0.1", 59999,
		withRuntimeVersion("1.0.0"),
	))

	assert.Nil(t, FindDaemonRuntime(dir), "expected no discoverable daemon")
	assert.True(t, IsDaemonActive(dir),
		"live runtime record should still suppress writes")
	assert.True(t, IsLocalDaemonActive(dir),
		"live writable runtime record should claim the SQLite archive")
}

func TestIsDaemonActive_DeadPIDDaemonRuntime(t *testing.T) {
	dir := runtimeTestDir(t)
	pid := deadPID(t)
	path, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		"127.0.0.1", 59994,
		withRuntimePID(pid),
	))
	require.NoError(t, err)

	assert.False(t, IsDaemonActive(dir), "expected false for dead PID runtime record")
	assertPathRemoved(t, path, "dead runtime record not cleaned up")
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
	startedAt := time.Now().UTC().Add(-time.Minute)
	endpoint, legacyPath := writeProbeableLegacyRuntime(t, dir, legacyStateFile{
		Version:   "legacy",
		StartedAt: startedAt.Format(time.RFC3339Nano),
	})

	result := FindDaemonRuntime(dir)
	require.Nil(t, result, "legacy state without compatibility metadata is incompatible")
	incompatible := requireMigratedIncompatibleRuntime(t, dir)
	assert.Equal(t, os.Getpid(), incompatible.Record.PID)
	assert.Equal(t, endpoint.Host, incompatible.Host)
	assert.Equal(t, endpoint.Port, incompatible.Port)
	assert.Equal(t, "legacy", incompatible.Record.Version)
	assert.False(t, incompatible.ReadOnly)
	assert.True(t, IsDaemonActive(dir))
	assert.True(t, IsLocalDaemonActive(dir))

	assertPathRemoved(t, legacyPath,
		"migrated legacy state file should be removed")
	runtimePath := runtimePathForTest(dir, os.Getpid())
	assert.FileExists(t, runtimePath, "kit runtime record should be written")
}

func TestFindDaemonRuntime_MigratesLegacyReadOnlyStateFile(t *testing.T) {
	dir := runtimeTestDir(t)
	_, legacyPath := writeProbeableLegacyRuntime(t, dir, legacyStateFile{
		ReadOnly: true,
	})

	result := FindDaemonRuntime(dir)
	require.Nil(t, result, "read-only legacy state without compatibility metadata is incompatible")
	incompatible := requireMigratedIncompatibleRuntime(t, dir)
	assert.True(t, incompatible.ReadOnly)
	assert.False(t, IsLocalDaemonActive(dir))
	assert.True(t, IsDaemonActive(dir))
	assertPathRemoved(t, legacyPath,
		"migrated legacy state file should be removed")
}

func TestFindDaemonRuntime_LegacyStateFileUsesPortFromName(t *testing.T) {
	dir := runtimeTestDir(t)
	endpoint, legacyPath := writeProbeableLegacyRuntime(t, dir, legacyStateFile{})
	rewriteLegacyState(t, legacyPath, func(state *legacyStateFile) {
		state.Port = 0
	})

	result, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, result, "expected legacy port from file name to migrate")
	require.Error(t, compatErr)
	assert.Equal(t, endpoint.Port, result.Port)
}

func TestIsLocalDaemonActive_LegacyDeadPIDStateFileRemoved(t *testing.T) {
	dir := runtimeTestDir(t)
	pid := deadPID(t)
	path := writeLegacyRuntimeStateForTest(t, dir, legacyStateFile{PID: pid})

	assert.False(t, IsLocalDaemonActive(dir))
	assertPathRemoved(t, path, "dead legacy state file not removed")
	assertRuntimeRecordRemoved(t, dir, pid,
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
	assert.FileExists(t, path, "unprobeable live legacy state should be left intact")
	assertRuntimeRecordRemoved(t, dir, os.Getpid(),
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
	_, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		"127.0.0.1", 9,
		withRuntimeMetadata(
			runtimeCreateTime, strconv.FormatInt(liveCreateTime+1, 10),
		),
	))
	require.NoError(t, err)

	assert.False(t, hasLiveWritableDaemonRuntime(dir))
	assert.False(t, IsLocalDaemonActive(dir))
	assertRuntimeRecordRemoved(t, dir, os.Getpid(),
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
	writeLiveRuntime(t, dir, false, withRuntimeVersion("1.0.0"))

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

	rec := readRuntimeRecord(t, path)
	assert.Equal(t, "true", rec.Metadata[runtimeReadOnly])
	assert.Equal(t, strconv.Itoa(port), rec.Metadata[runtimePort])
	assert.Equal(t, "test", rec.Version)
}
