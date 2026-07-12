package main

import (
	"bufio"
	"encoding/json"
	"errors"
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
	"go.kenn.io/agentsview/internal/config"
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
	cmd := exec.Command("sh", "-c", "trap '' TERM; echo ready; exec sleep 60")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready", strings.TrimSpace(ready))
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// startReapedTERMIgnoringProcess starts a child that has installed its SIGTERM
// disposition before returning. The child is reaped concurrently so a test can
// distinguish a running process from one that stopDaemonProcess force-killed.
func startReapedTERMIgnoringProcess(t *testing.T) (int, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "trap '' TERM; echo ready; exec sleep 60")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready", strings.TrimSpace(ready))

	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})
	return cmd.Process.Pid, reaped
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

func withRuntimeStartedAt(startedAt time.Time) runtimeRecordOption {
	return func(rec *daemon.RuntimeRecord) { rec.StartedAt = startedAt }
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

// TestSameProcessStartMarkerNeverReportsExternal hammers the race between
// markDaemonStarting's flock-acquire-then-register sequence and the
// isExternalDaemonStarting probe: a start marker owned by this process must
// never be classified as an external daemon startup, even mid-acquisition.
// Misclassification makes waitForExternalServeStartup return before the
// same-process owner publishes its runtime record, which surfaced as a flaky
// "serve --background is already in progress" error in
// TestEnsureBackgroundServeConcurrentLaunchConvergesOnDaemon.
func TestSameProcessStartMarkerNeverReportsExternal(t *testing.T) {
	dir := runtimeTestDir(t)
	require.NoError(t, os.MkdirAll(dir, 0o700))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			MarkDaemonStarting(dir)
			UnmarkDaemonStarting(dir)
		}
	})
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
		UnmarkDaemonStarting(dir)
	})

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		require.False(t, isExternalDaemonStarting(dir),
			"same-process start marker reported as external")
	}
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

func newPingDaemonWithProbeSignal(t *testing.T) (testDaemonEndpoint, <-chan struct{}) {
	t.Helper()
	pinged := make(chan struct{})
	var pingedOnce sync.Once
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pingedOnce.Do(func() { close(pinged) })
		ping.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return serverEndpoint(t, ts), pinged
}

func testPingServer(t *testing.T) (host string, port int) {
	t.Helper()
	endpoint := newPingDaemon(t)
	return endpoint.Host, endpoint.Port
}

func writeStartupFallbackFixture(
	t *testing.T, dir, host string, port, pid int, createTime string,
) {
	t.Helper()
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	state := startupState{
		PID:          pid,
		StartedAt:    time.Now().Add(-time.Minute),
		Phase:        "starting HTTP server",
		Host:         host,
		Port:         port,
		RuntimeError: "permission denied writing runtime record",
		CreateTime:   createTime,
		APIVersion:   daemonAPIVersion,
		DataVersion:  db.CurrentDataVersion(),
		UpdatedAt:    time.Now(),
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(startupStatePath(dir), data, 0o600))
}

func failRuntimeRecordListing(t *testing.T) {
	t.Helper()
	oldList := listDaemonRuntimeRecords
	listDaemonRuntimeRecords = func(daemon.RuntimeStore) ([]daemon.RuntimeRecord, error) {
		return nil, errors.New("store unavailable")
	}
	t.Cleanup(func() { listDaemonRuntimeRecords = oldList })
}

func TestFindDaemonRuntime_UsesStartupFallbackWhenRuntimeStoreInspectionFails(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeStartupFallbackFixture(
		t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10),
	)
	failRuntimeRecordListing(t)

	rt := FindDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.True(t, rt.RuntimeFallback)
	assert.Equal(t, port, rt.Port)
}

func TestFindIncompatibleDaemonRuntime_UsesStartupFallbackWhenRuntimeStoreInspectionFails(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeStartupFallbackFixture(
		t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10),
	)
	state := readStartupState(dir)
	require.NotNil(t, state)
	state.APIVersion = daemonAPIVersion + 1
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(startupStatePath(dir), data, 0o600))
	failRuntimeRecordListing(t)

	rt, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, rt)
	require.Error(t, compatErr)
	assert.True(t, rt.RuntimeFallback)
	assert.ErrorContains(t, compatErr, "API version")
}

func TestWritableDaemonFallbackResolver(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10))

	rt := FindWritableDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.True(t, rt.RuntimeFallback)
	assert.Equal(t, port, rt.Port)

	state := readStartupState(dir)
	require.NotNil(t, state)
	state.CreateTime = "1"
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(startupStatePath(dir), data, 0o600))
	assert.Nil(t, FindWritableDaemonRuntime(dir), "stale fallback must fail closed")
}

func TestFindWritableDaemonRuntime_StartupFallbackSurvivesStartLockProbeError(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	state := startupState{
		PID:          os.Getpid(),
		StartedAt:    time.Now().Add(-time.Minute),
		Phase:        "starting HTTP server",
		Host:         host,
		Port:         port,
		RuntimeError: "permission denied writing runtime record",
		CreateTime:   strconv.FormatInt(createTime, 10),
		APIVersion:   daemonAPIVersion,
		DataVersion:  db.CurrentDataVersion(),
		UpdatedAt:    time.Now(),
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(startupStatePath(dir), data, 0o600))

	oldTryLock := startLockTryLock
	startLockTryLock = func(*flock.Flock) (bool, error) {
		return false, errors.New("simulated lock probe failure")
	}
	t.Cleanup(func() { startLockTryLock = oldTryLock })

	rt := FindWritableDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.True(t, rt.RuntimeFallback)
	assert.True(t, IsLocalDaemonActive(dir))
}

func TestLocalWritableDaemonRecordsWithFallbackPreservesReadOnlyRecords(t *testing.T) {
	dir := runtimeTestDir(t)
	readOnlyHost, readOnlyPort := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeRuntimeRecordFixture(t, dir, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Service:   daemonService,
		Version:   "test",
		Network:   daemon.NetworkTCP,
		Address:   net.JoinHostPort(readOnlyHost, strconv.Itoa(readOnlyPort)),
		StartedAt: time.Now(),
		Metadata: map[string]string{
			runtimeHost:        readOnlyHost,
			runtimePort:        strconv.Itoa(readOnlyPort),
			runtimeReadOnly:    "true",
			runtimeAPIVersion:  strconv.Itoa(daemonAPIVersion),
			runtimeDataVersion: strconv.Itoa(db.CurrentDataVersion()),
			runtimeCreateTime:  strconv.FormatInt(createTime, 10),
		},
	})
	host, port := testPingServer(t)
	writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10))

	records, fallback := localWritableDaemonRecordsWithFallback(dir, "")
	require.True(t, fallback)
	require.Len(t, records, 2)

	readOnlySeen := false
	writableSeen := false
	for _, rec := range records {
		rt := daemonRuntimeFromRecord(rec)
		if rt.ReadOnly {
			readOnlySeen = true
			assert.Equal(t, readOnlyPort, rt.Port)
			continue
		}
		writableSeen = true
		assert.Equal(t, port, rt.Port)
	}
	assert.True(t, readOnlySeen)
	assert.True(t, writableSeen)
}

func TestFindDaemonRuntime_IgnoresIncompatibleStartupStateFallback(t *testing.T) {
	dir := runtimeTestDir(t)
	host, port := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeStartupFallbackFixture(
		t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10),
	)
	state := readStartupState(dir)
	require.NotNil(t, state)
	state.APIVersion = daemonAPIVersion + 1
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(startupStatePath(dir), data, 0o600))

	assert.Nil(t, FindDaemonRuntime(dir))
	rt, compatErr := FindIncompatibleDaemonRuntime(dir)
	require.NotNil(t, rt)
	require.Error(t, compatErr)
	assert.Contains(t, compatErr.Error(), "API version")
	assert.True(t, rt.RuntimeFallback)
}

func TestWriteDaemonRuntimeFailurePreservesUpdateLaunchArgs(t *testing.T) {
	dir := runtimeTestDir(t)
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	newStartupStateWriter(dir, time.Now).SetPhase("starting HTTP server")
	host, port := testPingServer(t)
	runtimePath, err := runtimeStore(dir).Path(os.Getpid())
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(runtimePath, 0o700))

	_, err = WriteDaemonRuntimeWithAuthAndNoSync(
		dir, host, port, "test", false, true, true,
	)
	require.Error(t, err)

	rt := FindWritableDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.Equal(t, "test", rt.Record.Version)
	state := readStartupState(dir)
	require.NotNil(t, state)
	assert.True(t, state.RequireAuthKnown)
	assert.True(t, state.RequireAuth)
	assert.True(t, state.NoSyncKnown)
	assert.True(t, state.NoSync)

	oldStop := stopDaemonRuntimeForUpgrade
	stopDaemonRuntimeForUpgrade = func(_ config.Config, _ *DaemonRuntime) error { return nil }
	t.Cleanup(func() { stopDaemonRuntimeForUpgrade = oldStop })
	result, err := stopWritableDaemonsForUpdate(config.Config{DataDir: dir})
	require.NoError(t, err)
	assert.True(t, result.Stopped)
	args := restartDaemonAfterUpdateArgs(config.Config{}, result)
	assert.Contains(t, args, "--require-auth")
	assert.Contains(t, args, "--no-sync")
}

func TestWriteDaemonRuntimeFailurePreservesManagedCaddyIdentity(t *testing.T) {
	dir := runtimeTestDir(t)
	MarkDaemonStarting(dir)
	t.Cleanup(func() { UnmarkDaemonStarting(dir) })
	newStartupStateWriter(dir, time.Now).SetPhase("starting HTTP server")
	host, port := testPingServer(t)
	runtimePath, err := runtimeStore(dir).Path(os.Getpid())
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(runtimePath, 0o700))
	caddyCreateTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)

	_, err = WriteDaemonRuntimeWithAuthAndNoSync(
		dir, host, port, "test", false, false, false, os.Getpid(),
	)
	require.Error(t, err)

	state := readStartupState(dir)
	require.NotNil(t, state)
	assert.Equal(t, os.Getpid(), state.CaddyPID)
	assert.Equal(t, strconv.FormatInt(caddyCreateTime, 10), state.CaddyCreateTime)

	rt := FindWritableDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.Equal(t, strconv.Itoa(os.Getpid()), rt.Record.Metadata[runtimeCaddyPID])
	assert.Equal(t, strconv.FormatInt(caddyCreateTime, 10), rt.Record.Metadata[runtimeCaddyCreateTime])
}

func TestStopWritableDaemonsForUpdateIgnoresStaleWritableRecordBeforeFallback(t *testing.T) {
	dir := runtimeTestDir(t)
	staleHost, stalePort := testPingServer(t)
	createTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	writeRuntimeRecordFixture(t, dir, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Service:   daemonService,
		Version:   "stale",
		Network:   daemon.NetworkTCP,
		Address:   net.JoinHostPort(staleHost, strconv.Itoa(stalePort)),
		StartedAt: time.Now(),
		Metadata: map[string]string{
			runtimeHost:        staleHost,
			runtimePort:        strconv.Itoa(stalePort),
			runtimeAPIVersion:  strconv.Itoa(daemonAPIVersion),
			runtimeDataVersion: strconv.Itoa(db.CurrentDataVersion()),
			runtimeCreateTime:  "1",
		},
	})
	host, port := testPingServer(t)
	writeStartupFallbackFixture(
		t, dir, host, port, os.Getpid(), strconv.FormatInt(createTime, 10),
	)

	oldStop := stopDaemonRuntimeForUpgrade
	var stopped *DaemonRuntime
	stopDaemonRuntimeForUpgrade = func(_ config.Config, rt *DaemonRuntime) error {
		stopped = rt
		return nil
	}
	t.Cleanup(func() { stopDaemonRuntimeForUpgrade = oldStop })

	result, err := stopWritableDaemonsForUpdate(config.Config{DataDir: dir})
	require.NoError(t, err)
	require.NotNil(t, stopped)
	assert.True(t, result.Stopped)
	assert.Equal(t, port, stopped.Port)
	assert.Equal(t, strconv.FormatInt(createTime, 10), stopped.Record.Metadata[runtimeCreateTime])
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

func writeAuthenticatedProbeableLegacyRuntime(
	t *testing.T,
	dataDir string,
	token string,
	state legacyStateFile,
) (testDaemonEndpoint, string) {
	t.Helper()
	endpoint := newAuthenticatedPingDaemon(t, token)
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

func TestCompareProcessCreateTime(t *testing.T) {
	tests := []struct {
		name     string
		recorded string
		live     int64
		liveOK   bool
		want     processCreateTimeState
	}{
		{name: "exact match", recorded: "1234", live: 1234, liveOK: true, want: processCreateTimeMatch},
		{name: "proven mismatch", recorded: "1234", live: 1235, liveOK: true, want: processCreateTimeMismatch},
		{name: "missing recorded value", recorded: "", live: 1234, liveOK: true, want: processCreateTimeUnknown},
		{name: "malformed recorded value", recorded: "not-a-time", live: 1234, liveOK: true, want: processCreateTimeUnknown},
		{name: "zero recorded value", recorded: "0", live: 1234, liveOK: true, want: processCreateTimeUnknown},
		{name: "negative recorded value", recorded: "-1", live: 1234, liveOK: true, want: processCreateTimeUnknown},
		{name: "live lookup unavailable", recorded: "1234", live: 0, liveOK: false, want: processCreateTimeUnknown},
		{name: "nonpositive live value", recorded: "1234", live: 0, liveOK: true, want: processCreateTimeUnknown},
		{name: "negative live value", recorded: "1234", live: -1, liveOK: true, want: processCreateTimeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, compareProcessCreateTime(
				tt.recorded, tt.live, tt.liveOK,
			))
		})
	}
}

func TestWritableDaemonRecordsFiltersAndCleansRuntimeRecords(t *testing.T) {
	liveCreateTime, ok := processCreateTimeMillis(os.Getpid())
	if !ok {
		t.Skip("process create time is unavailable on this platform")
	}

	tests := []struct {
		name        string
		options     []runtimeRecordOption
		wantRecords int
		wantFile    bool
	}{
		{
			name:        "matching writable record",
			options:     []runtimeRecordOption{withRuntimeMetadata(runtimeCreateTime, strconv.FormatInt(liveCreateTime, 10))},
			wantRecords: 1,
			wantFile:    true,
		},
		{
			name:        "missing create time is preserved as unknown",
			wantRecords: 1,
			wantFile:    true,
		},
		{
			name:        "malformed create time is preserved as unknown",
			options:     []runtimeRecordOption{withRuntimeMetadata(runtimeCreateTime, "not-a-time")},
			wantRecords: 1,
			wantFile:    true,
		},
		{
			name:        "zero create time is preserved as unknown",
			options:     []runtimeRecordOption{withRuntimeMetadata(runtimeCreateTime, "0")},
			wantRecords: 1,
			wantFile:    true,
		},
		{
			name:        "negative create time is preserved as unknown",
			options:     []runtimeRecordOption{withRuntimeMetadata(runtimeCreateTime, "-1")},
			wantRecords: 1,
			wantFile:    true,
		},
		{
			name:        "proven mismatch is removed",
			options:     []runtimeRecordOption{withRuntimeMetadata(runtimeCreateTime, strconv.FormatInt(liveCreateTime+1, 10))},
			wantRecords: 0,
			wantFile:    false,
		},
		{
			name:        "read only record is ignored but preserved",
			options:     []runtimeRecordOption{withRuntimeReadOnly(true)},
			wantRecords: 0,
			wantFile:    true,
		},
		{
			name:        "another service is ignored but preserved",
			options:     []runtimeRecordOption{func(rec *daemon.RuntimeRecord) { rec.Service = "other" }},
			wantRecords: 0,
			wantFile:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := runtimeTestDir(t)
			path, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
				"127.0.0.1", 9, tt.options...,
			))
			require.NoError(t, err)

			records, err := writableDaemonRecords(dir, "")
			require.NoError(t, err)
			require.Len(t, records, tt.wantRecords)
			if tt.wantRecords > 0 {
				assert.Equal(t, os.Getpid(), records[0].PID)
				assert.Equal(t, path, records[0].SourcePath)
			}
			if tt.wantFile {
				assert.FileExists(t, path)
			} else {
				assertPathRemoved(t, path)
			}
		})
	}
}

func TestWritableDaemonRecordsCleansDeadRecord(t *testing.T) {
	dir := runtimeTestDir(t)
	pid := deadPID(t)
	path, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		"127.0.0.1", 9, withRuntimePID(pid),
	))
	require.NoError(t, err)

	records, err := writableDaemonRecords(dir, "")
	require.NoError(t, err)
	assert.Empty(t, records)
	assertPathRemoved(t, path, "dead runtime record should be cleaned up")
}

func TestWritableDaemonRecordsReturnsEveryLiveWritableRecord(t *testing.T) {
	requirePOSIXSignals(t, "requires a long-lived child process")
	dir := runtimeTestDir(t)
	pids := []int{os.Getpid(), startSleepProcess(t)}
	wantPaths := make(map[int]string, len(pids))
	for i, pid := range pids {
		path, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
			"127.0.0.1", 10+i, withRuntimePID(pid),
		))
		require.NoError(t, err)
		wantPaths[pid] = path
	}

	records, err := writableDaemonRecords(dir, "")
	require.NoError(t, err)
	require.Len(t, records, len(pids))
	for _, rec := range records {
		assert.Equal(t, wantPaths[rec.PID], rec.SourcePath)
		delete(wantPaths, rec.PID)
	}
	assert.Empty(t, wantPaths, "all live writable records should be returned")
}

func TestWritableDaemonRecordsMigratesLegacyRuntime(t *testing.T) {
	dir := runtimeTestDir(t)
	endpoint, legacyPath := writeProbeableLegacyRuntime(t, dir, legacyStateFile{})

	records, err := writableDaemonRecords(dir, "")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, endpoint.Port, daemonRuntimeFromRecord(records[0]).Port)
	assert.Equal(t, runtimePathForTest(dir, os.Getpid()), records[0].SourcePath)
	assertPathRemoved(t, legacyPath, "migrated legacy record should be removed")
}

func TestWritableDaemonRecordsReportsMismatchedRecordRemovalFailure(
	t *testing.T,
) {
	requirePOSIXSignals(t, "directory removal permissions require POSIX semantics")
	dir := runtimeTestDir(t)
	liveCreateTime, ok := processCreateTimeMillis(os.Getpid())
	require.True(t, ok)
	path, err := writeRuntimeRecordForTest(dir, daemonRuntimeRecord(
		"127.0.0.1", 9,
		withRuntimeMetadata(
			runtimeCreateTime, strconv.FormatInt(liveCreateTime+1, 10),
		),
	))
	require.NoError(t, err)
	stored, err := runtimeStore(dir).List()
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	records, err := writableDaemonRecordsFromStore(
		staticRuntimeRecordStore{records: stored},
	)
	assert.Nil(t, records)
	require.Error(t, err)
	assert.ErrorContains(t, err, "remove mismatched daemon runtime record")
	assert.ErrorContains(t, err, path)
	assert.FileExists(t, path, "failed cleanup must leave the record recoverable")
}

type staticRuntimeRecordStore struct {
	records []daemon.RuntimeRecord
}

func (staticRuntimeRecordStore) CleanupDead() (int, error) {
	return 0, nil
}

func (s staticRuntimeRecordStore) List() ([]daemon.RuntimeRecord, error) {
	return s.records, nil
}

type cleanupFailingRuntimeRecordStore struct {
	err error
}

func (s cleanupFailingRuntimeRecordStore) CleanupDead() (int, error) {
	return 0, s.err
}

func (cleanupFailingRuntimeRecordStore) List() ([]daemon.RuntimeRecord, error) {
	return nil, nil
}

type listFailingRuntimeRecordStore struct {
	err error
}

func (listFailingRuntimeRecordStore) CleanupDead() (int, error) {
	return 0, nil
}

func (s listFailingRuntimeRecordStore) List() ([]daemon.RuntimeRecord, error) {
	return nil, s.err
}

func TestWritableDaemonRecordsSurfacesRuntimeStoreErrors(t *testing.T) {
	tests := []struct {
		name  string
		store daemonRuntimeRecordStore
		want  string
	}{
		{
			name:  "cleanup failure",
			store: cleanupFailingRuntimeRecordStore{err: errors.New("cleanup failed")},
			want:  "cleanup failed",
		},
		{
			name:  "list failure",
			store: listFailingRuntimeRecordStore{err: errors.New("list failed")},
			want:  "list failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			records, err := writableDaemonRecordsFromStore(tt.store)
			assert.Nil(t, records)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
		})
	}
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
