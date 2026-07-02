// ABOUTME: Adapts kit daemon runtime records for agentsview CLI transport.
// ABOUTME: Keeps daemon discovery metadata close to commands that use it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"github.com/shirou/gopsutil/v4/process"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/daemon"
)

const (
	daemonService          = "agentsview"
	daemonAPIVersion       = 2
	runtimeReadOnly        = "read_only"
	runtimeHost            = "host"
	runtimePort            = "port"
	runtimeRequireAuth     = "require_auth"
	runtimeNoSync          = "no_sync"
	runtimeAPIVersion      = "api_version"
	runtimeDataVersion     = "data_version"
	runtimeCreateTime      = "create_time"
	runtimeCaddyPID        = "caddy_pid"
	runtimeCaddyCreateTime = "caddy_create_time"
	defaultStartProbeTick  = 250 * time.Millisecond
)

var startProbeTickNanos int64 = int64(defaultStartProbeTick)

func startProbeTick() time.Duration {
	return time.Duration(atomic.LoadInt64(&startProbeTickNanos))
}

// DaemonRuntime is the agentsview-specific view of a kit daemon runtime record.
type DaemonRuntime struct {
	Record           daemon.RuntimeRecord
	Host             string
	Port             int
	ReadOnly         bool
	RequireAuth      bool
	RequireAuthKnown bool
	NoSync           bool
	API              int
	Data             int
}

func runtimeStore(dataDir string) daemon.RuntimeStore {
	return daemon.RuntimeStore{Dir: dataDir}
}

// WriteDaemonRuntime writes a shared kit daemon runtime record for the running
// server. It returns the path written. The optional caddyPID records a managed
// Caddy child so `serve stop` can terminate it if the server is force-killed
// before it can stop Caddy itself.
func WriteDaemonRuntime(
	dataDir string, host string, port int, version string,
	readOnly bool, caddyPID ...int,
) (string, error) {
	return WriteDaemonRuntimeWithAuth(
		dataDir, host, port, version, readOnly, false, caddyPID...,
	)
}

func WriteDaemonRuntimeWithAuth(
	dataDir string, host string, port int, version string,
	readOnly bool, requireAuth bool, caddyPID ...int,
) (string, error) {
	return WriteDaemonRuntimeWithAuthAndNoSync(
		dataDir, host, port, version, readOnly, requireAuth, false,
		caddyPID...,
	)
}

func WriteDaemonRuntimeWithAuthAndNoSync(
	dataDir string, host string, port int, version string,
	readOnly bool, requireAuth bool, noSync bool, caddyPID ...int,
) (string, error) {
	ep := daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(probeHostForDial(host), strconv.Itoa(port)),
	}
	rec := daemon.NewRuntimeRecord(daemonService, version, ep)
	rec.Metadata = map[string]string{
		runtimeHost:        host,
		runtimePort:        strconv.Itoa(port),
		runtimeReadOnly:    strconv.FormatBool(readOnly),
		runtimeRequireAuth: strconv.FormatBool(requireAuth),
		runtimeNoSync:      strconv.FormatBool(noSync),
		runtimeAPIVersion:  strconv.Itoa(daemonAPIVersion),
		runtimeDataVersion: strconv.Itoa(db.CurrentDataVersion()),
	}
	// Persist this process's OS create time so `serve stop` can confirm a
	// PID still belongs to the recorded daemon (and was not reused) by
	// matching create times exactly. Best-effort: if it cannot be read, stop
	// falls back to ping confirmation only.
	if ct, ok := processCreateTimeMillis(os.Getpid()); ok {
		rec.Metadata[runtimeCreateTime] = strconv.FormatInt(ct, 10)
	}
	if len(caddyPID) > 0 && caddyPID[0] > 0 {
		rec.Metadata[runtimeCaddyPID] = strconv.Itoa(caddyPID[0])
		if ct, ok := processCreateTimeMillis(caddyPID[0]); ok {
			rec.Metadata[runtimeCaddyCreateTime] = strconv.FormatInt(ct, 10)
		}
	}
	return runtimeStore(dataDir).Write(rec)
}

// processCreateTimeMillis returns the OS-reported create time of pid in
// milliseconds since the Unix epoch. ok is false when the process is gone or
// its create time cannot be read.
func processCreateTimeMillis(pid int) (int64, bool) {
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, false
	}
	created, err := proc.CreateTime()
	if err != nil {
		return 0, false
	}
	return created, true
}

// RemoveDaemonRuntime removes the current process's kit daemon runtime record.
func RemoveDaemonRuntime(dataDir string) {
	path, err := runtimeStore(dataDir).Path(os.Getpid())
	if err == nil {
		_ = os.Remove(path)
	}
}

// FindDaemonRuntime returns a live agentsview daemon whose kit runtime record
// passes the ping probe. Writable daemons are preferred over read-only pg serve
// daemons when both are discoverable. When authToken is non-empty, it is sent
// as a bearer token so require_auth daemons remain discoverable.
func FindDaemonRuntime(dataDir string, authToken ...string) *DaemonRuntime {
	migrateLegacyDaemonRuntimes(dataDir, authToken...)

	store := runtimeStore(dataDir)
	_, _ = store.CleanupDead()

	records, err := store.List()
	if err != nil {
		return nil
	}

	ctx := context.Background()
	token := firstAuthToken(authToken)
	var readOnly *DaemonRuntime
	for _, rec := range records {
		if rec.Service != "" && rec.Service != daemonService {
			continue
		}
		if !daemon.ProcessAlive(rec.PID) {
			continue
		}
		if runtimeRecordHasMismatchedCreateTime(store, rec) {
			continue
		}
		info, err := probeRuntime(ctx, rec, token, daemon.ProbeOptions{
			ExpectedService: daemonService,
			Timeout:         500 * time.Millisecond,
		})
		if err != nil || info.PID != rec.PID {
			continue
		}
		rt := daemonRuntimeFromRecord(rec)
		if daemonRuntimeCompatibilityError(rt) != nil {
			continue
		}
		if !rt.ReadOnly {
			return rt
		}
		if readOnly == nil {
			readOnly = rt
		}
	}
	return readOnly
}

func FindIncompatibleDaemonRuntime(
	dataDir string, authToken ...string,
) (*DaemonRuntime, error) {
	rt := findIncompatibleDaemonRuntime(dataDir, firstAuthToken(authToken))
	if rt == nil {
		return nil, nil
	}
	return rt, daemonRuntimeCompatibilityError(rt)
}

func findIncompatibleWritableDaemonRuntime(
	dataDir string, authToken ...string,
) (*DaemonRuntime, error) {
	rt, err := FindIncompatibleDaemonRuntime(dataDir, authToken...)
	if rt != nil && rt.ReadOnly {
		return nil, nil
	}
	return rt, err
}

func findIncompatibleDaemonRuntime(
	dataDir string, token string,
) *DaemonRuntime {
	migrateLegacyDaemonRuntimes(dataDir, token)

	store := runtimeStore(dataDir)
	_, _ = store.CleanupDead()
	records, err := store.List()
	if err != nil {
		return nil
	}

	ctx := context.Background()
	var readOnly *DaemonRuntime
	for _, rec := range records {
		if rec.Service != "" && rec.Service != daemonService {
			continue
		}
		if !daemon.ProcessAlive(rec.PID) {
			continue
		}
		if runtimeRecordHasMismatchedCreateTime(store, rec) {
			continue
		}
		info, err := probeRuntime(ctx, rec, token, daemon.ProbeOptions{
			ExpectedService: daemonService,
			Timeout:         500 * time.Millisecond,
		})
		if err != nil || info.PID != rec.PID {
			continue
		}
		rt := daemonRuntimeFromRecord(rec)
		if daemonRuntimeCompatibilityError(rt) == nil {
			continue
		}
		if !rt.ReadOnly {
			return rt
		}
		if readOnly == nil {
			readOnly = rt
		}
	}
	return readOnly
}

func firstAuthToken(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0]
}

func probeRuntime(
	ctx context.Context,
	rec daemon.RuntimeRecord,
	authToken string,
	opts daemon.ProbeOptions,
) (daemon.PingInfo, error) {
	ep := rec.Endpoint()
	if authToken == "" {
		return daemon.Probe(ctx, ep, opts)
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := ep.HTTPClient(daemon.HTTPClientOptions{
		Timeout:           timeout,
		DisableKeepAlives: true,
	})
	client.Transport = bearerAuthTransport{
		token: authToken,
		base:  client.Transport,
	}
	return daemon.ProbeHTTP(ctx, client, ep.BaseURL(), opts)
}

type bearerAuthTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerAuthTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func daemonRuntimeFromRecord(rec daemon.RuntimeRecord) *DaemonRuntime {
	ep := rec.Endpoint()
	host, portText, _ := net.SplitHostPort(ep.Address)
	port, _ := strconv.Atoi(portText)
	if rec.Metadata != nil {
		if h := rec.Metadata[runtimeHost]; h != "" {
			host = h
		}
		if p := rec.Metadata[runtimePort]; p != "" {
			if parsed, err := strconv.Atoi(p); err == nil {
				port = parsed
			}
		}
	}
	readOnly := false
	requireAuth := false
	requireAuthKnown := false
	noSync := false
	apiVersion := 0
	dataVersion := 0
	if rec.Metadata != nil {
		readOnly, _ = strconv.ParseBool(rec.Metadata[runtimeReadOnly])
		if raw, ok := rec.Metadata[runtimeRequireAuth]; ok {
			requireAuth, _ = strconv.ParseBool(raw)
			requireAuthKnown = true
		}
		noSync, _ = strconv.ParseBool(rec.Metadata[runtimeNoSync])
		apiVersion, _ = strconv.Atoi(rec.Metadata[runtimeAPIVersion])
		dataVersion, _ = strconv.Atoi(rec.Metadata[runtimeDataVersion])
	}
	return &DaemonRuntime{
		Record:           rec,
		Port:             port,
		Host:             host,
		ReadOnly:         readOnly,
		RequireAuth:      requireAuth,
		RequireAuthKnown: requireAuthKnown,
		NoSync:           noSync,
		API:              apiVersion,
		Data:             dataVersion,
	}
}

func daemonRuntimeCompatibilityError(rt *DaemonRuntime) error {
	if rt == nil {
		return nil
	}
	if rt.API != daemonAPIVersion {
		return fmt.Errorf(
			"daemon API version %d is incompatible with client API version %d",
			rt.API, daemonAPIVersion,
		)
	}
	if rt.Data != db.CurrentDataVersion() {
		return fmt.Errorf(
			"daemon data version %d is incompatible with client data version %d",
			rt.Data, db.CurrentDataVersion(),
		)
	}
	return nil
}

// liveDaemonRecords returns runtime records for agentsview daemons in dataDir
// whose process is still alive. Unlike FindDaemonRuntime it does not require a
// successful ping, so it can target a hung-but-alive server (e.g. for stop).
func liveDaemonRecords(dataDir string) []daemon.RuntimeRecord {
	migrateLegacyDaemonRuntimes(dataDir)

	store := runtimeStore(dataDir)
	_, _ = store.CleanupDead()
	records, err := store.List()
	if err != nil {
		return nil
	}
	var alive []daemon.RuntimeRecord
	for _, rec := range records {
		if rec.Service != "" && rec.Service != daemonService {
			continue
		}
		if !daemon.ProcessAlive(rec.PID) {
			continue
		}
		alive = append(alive, rec)
	}
	return alive
}

func hasLiveDaemonRuntime(dataDir string, authToken ...string) bool {
	migrateLegacyDaemonRuntimes(dataDir, authToken...)

	store := runtimeStore(dataDir)
	_, _ = store.CleanupDead()
	records, err := store.List()
	if err != nil {
		return false
	}
	for _, rec := range records {
		if rec.Service != "" && rec.Service != daemonService {
			continue
		}
		if !daemon.ProcessAlive(rec.PID) {
			continue
		}
		if runtimeRecordHasMismatchedCreateTime(store, rec) {
			continue
		}
		return true
	}
	return false
}

type legacyStateFile struct {
	PID       int    `json:"pid"`
	Port      int    `json:"port,omitempty"`
	Host      string `json:"host,omitempty"`
	Version   string `json:"version,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

func isLegacyStateFileName(name string) bool {
	return strings.HasPrefix(name, "server.") &&
		strings.HasSuffix(name, ".json")
}

func migrateLegacyDaemonRuntimes(dataDir string, authToken ...string) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	token := firstAuthToken(authToken)
	for _, entry := range entries {
		if !isLegacyStateFileName(entry.Name()) {
			continue
		}
		path := filepath.Join(dataDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var sf legacyStateFile
		if err := json.Unmarshal(data, &sf); err != nil {
			continue
		}
		if !daemon.ProcessAlive(sf.PID) {
			_ = os.Remove(path)
			continue
		}
		if sf.Port <= 0 {
			sf.Port = legacyPortFromStateFileName(entry.Name())
		}
		if sf.Port <= 0 {
			continue
		}
		rec := legacyRuntimeRecord(sf)
		info, err := probeRuntime(context.Background(), rec, token, daemon.ProbeOptions{
			ExpectedService: daemonService,
			Timeout:         500 * time.Millisecond,
		})
		if err != nil || info.PID != sf.PID {
			continue
		}
		if rec.Version == "" {
			rec.Version = info.Version
		}
		if _, err := runtimeStore(dataDir).Write(rec); err != nil {
			continue
		}
		_ = os.Remove(path)
	}
}

func legacyPortFromStateFileName(name string) int {
	portText := strings.TrimSuffix(strings.TrimPrefix(name, "server."), ".json")
	port, _ := strconv.Atoi(portText)
	return port
}

func legacyRuntimeRecord(sf legacyStateFile) daemon.RuntimeRecord {
	host := sf.Host
	if host == "" {
		host = "127.0.0.1"
	}
	rec := daemon.RuntimeRecord{
		PID:     sf.PID,
		Network: daemon.NetworkTCP,
		Address: net.JoinHostPort(probeHostForDial(host), strconv.Itoa(sf.Port)),
		Service: daemonService,
		Version: sf.Version,
		Metadata: map[string]string{
			runtimeHost:     host,
			runtimePort:     strconv.Itoa(sf.Port),
			runtimeReadOnly: strconv.FormatBool(sf.ReadOnly),
		},
	}
	if sf.StartedAt != "" {
		if startedAt, err := time.Parse(time.RFC3339Nano, sf.StartedAt); err == nil {
			rec.StartedAt = startedAt.UTC()
		}
	}
	return rec
}

func hasLiveWritableDaemonRuntime(dataDir string, authToken ...string) bool {
	migrateLegacyDaemonRuntimes(dataDir, authToken...)

	store := runtimeStore(dataDir)
	_, _ = store.CleanupDead()
	records, err := store.List()
	if err != nil {
		return false
	}
	for _, rec := range records {
		if rec.Service != "" && rec.Service != daemonService {
			continue
		}
		if !daemon.ProcessAlive(rec.PID) {
			continue
		}
		if runtimeRecordHasMismatchedCreateTime(store, rec) {
			continue
		}
		if !daemonRuntimeFromRecord(rec).ReadOnly {
			return true
		}
	}
	return false
}

func runtimeRecordHasMismatchedCreateTime(
	store daemon.RuntimeStore,
	rec daemon.RuntimeRecord,
) bool {
	recorded := rec.Metadata[runtimeCreateTime]
	if recorded == "" || processCreateTimeMatches(rec.PID, recorded) {
		return false
	}
	if path, err := store.Path(rec.PID); err == nil {
		_ = os.Remove(path)
	}
	return true
}

type heldStartLock struct {
	path string
	lock *flock.Flock
}

var startLocks sync.Map

// startLockMu makes "this process holds the start flock" and "the lock is
// registered in startLocks" a single atomic state for same-process observers.
// Without it, a probe running between markDaemonStarting's flock acquire and
// its startLocks registration sees the lock held with no registration and
// misreports a same-process startup as an external daemon.
var startLockMu sync.Mutex

// markDaemonStarting acquires the kit daemon start lock for this data dir while
// the server is starting. owned reports whether this process now owns the
// marker; acquired reports whether this call acquired it.
func markDaemonStarting(dataDir string) (owned bool, acquired bool) {
	path, err := runtimeStore(dataDir).LockPath()
	if err != nil {
		return false, false
	}
	startLockMu.Lock()
	defer startLockMu.Unlock()
	if _, ok := startLocks.Load(path); ok {
		return true, false
	}
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return false, false
	}
	startLocks.Store(path, heldStartLock{path: path, lock: lock})
	return true, true
}

// MarkDaemonStarting acquires the kit daemon start lock for this data dir while
// the server is starting. The lock file itself is advisory; lock ownership is
// what other processes observe.
func MarkDaemonStarting(dataDir string) {
	markDaemonStarting(dataDir)
}

// UnmarkDaemonStarting releases the kit daemon start lock for this data dir.
func UnmarkDaemonStarting(dataDir string) {
	path, err := runtimeStore(dataDir).LockPath()
	if err != nil {
		return
	}
	startLockMu.Lock()
	defer startLockMu.Unlock()
	value, ok := startLocks.LoadAndDelete(path)
	if !ok {
		return
	}
	held := value.(heldStartLock)
	_ = held.lock.Unlock()
}

func isDaemonStarting(dataDir string) bool {
	path, err := runtimeStore(dataDir).LockPath()
	if err != nil {
		return false
	}
	startLockMu.Lock()
	defer startLockMu.Unlock()
	if _, ok := startLocks.Load(path); ok {
		return true
	}
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		return false
	}
	if locked {
		_ = lock.Unlock()
		return false
	}
	return true
}

func isExternalDaemonStarting(dataDir string) bool {
	path, err := runtimeStore(dataDir).LockPath()
	if err != nil {
		return false
	}
	startLockMu.Lock()
	defer startLockMu.Unlock()
	if _, ok := startLocks.Load(path); ok {
		return false
	}
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		// On Windows, probing a lock held by a helper process can report an
		// error instead of a clean locked=false result. Treat uncertainty as
		// active startup so replacement does not stop the incumbent daemon.
		return true
	}
	if locked {
		_ = lock.Unlock()
		return false
	}
	return true
}

const legacyStartupLockPrefix = "server.starting."

func isLegacyDaemonStarting(dataDir string) bool {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), legacyStartupLockPrefix) {
			continue
		}
		path := filepath.Join(dataDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
			continue
		}
		if !daemon.ProcessAlive(pid) {
			_ = os.Remove(path)
			continue
		}
		return true
	}
	return false
}

// IsDaemonStarting reports whether the shared kit daemon start lock is held.
func IsDaemonStarting(dataDir string) bool {
	return isDaemonStarting(dataDir) || isLegacyDaemonStarting(dataDir)
}

// IsDaemonActive reports whether a server process is managing dataDir.
func IsDaemonActive(dataDir string, authToken ...string) bool {
	return hasLiveDaemonRuntime(dataDir, authToken...) ||
		IsDaemonStarting(dataDir)
}

// IsLocalDaemonActive reports whether a writable local daemon is managing the
// SQLite archive in dataDir.
func IsLocalDaemonActive(dataDir string, authToken ...string) bool {
	return hasLiveWritableDaemonRuntime(dataDir, authToken...) ||
		IsDaemonStarting(dataDir)
}

// WaitForDaemonStartup polls until the daemon start lock clears or a running daemon is
// detected, up to the given timeout.
func WaitForDaemonStartup(
	dataDir string, timeout time.Duration, authToken ...string,
) bool {
	return WaitForDaemonStartupContext(
		context.Background(), dataDir, timeout, authToken...,
	)
}

func WaitForDaemonStartupContext(
	ctx context.Context,
	dataDir string,
	timeout time.Duration,
	authToken ...string,
) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if FindDaemonRuntime(dataDir, authToken...) != nil {
			return true
		}
		if !IsDaemonStarting(dataDir) {
			return false
		}
		wait := min(time.Until(deadline), startProbeTick())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

// probeHostForDial converts a bind-all address to a loopback address suitable
// for TCP readiness probes and daemon runtime endpoints.
func probeHostForDial(host string) string {
	switch host {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::":
		return "::1"
	default:
		return host
	}
}
