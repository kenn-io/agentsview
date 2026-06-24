package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/kit/daemon"
)

func TestRunRemoteHosts_AttemptsAllAndCollectsFailures(t *testing.T) {
	hosts := []config.RemoteHost{
		{Host: "alpha"},
		{Host: "beta", User: "u", Port: 2222},
		{Host: "gamma"},
	}
	failBeta := errors.New("ssh down")

	var attempted []config.RemoteHost
	failures := runRemoteHosts(hosts, true, func(rh config.RemoteHost, full bool) error {
		attempted = append(attempted, rh)
		assert.True(t, full, "full flag should propagate to syncFn")
		if rh.Host == "beta" {
			return failBeta
		}
		return nil
	})

	// Every host attempted, in declared order, even after a failure.
	require.Equal(t, hosts, attempted)
	// Only beta failed; its full RemoteHost (user/port) is preserved.
	require.Len(t, failures, 1)
	assert.Equal(t, hosts[1], failures[0].Host)
	assert.Equal(t, failBeta, failures[0].Err)
}

func TestRunRemoteHosts_AllSucceedReturnsEmpty(t *testing.T) {
	hosts := []config.RemoteHost{{Host: "alpha"}, {Host: "beta"}}
	failures := runRemoteHosts(hosts, false, func(config.RemoteHost, bool) error {
		return nil
	})
	assert.Empty(t, failures)
}

func TestSyncLocalAndRemotes_ResyncForcesRemoteFull(t *testing.T) {
	tests := []struct {
		name      string
		cfgFull   bool
		didResync bool
		wantFull  bool
	}{
		{"no full, no resync", false, false, false},
		{"automatic resync forces remote full", false, true, true},
		{"cli --full", true, false, true},
		{"both", true, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hosts := []config.RemoteHost{{Host: "alpha"}, {Host: "beta"}}
			localCalled := false
			var gotFull []bool
			failures := syncLocalAndRemotes(hosts, tt.cfgFull,
				func() bool { localCalled = true; return tt.didResync },
				func(_ config.RemoteHost, full bool) error {
					gotFull = append(gotFull, full)
					return nil
				})

			require.True(t, localCalled, "local sync must run")
			assert.Empty(t, failures)
			require.Len(t, gotFull, len(hosts))
			for _, full := range gotFull {
				assert.Equal(t, tt.wantFull, full)
			}
		})
	}
}

func TestUseDaemonForSync(t *testing.T) {
	tests := []struct {
		name     string
		readOnly bool
		want     bool
	}{
		{"skips read-only daemon", true, false},
		{"uses writable daemon", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			use := useDaemonForSync(transport{
				Mode:     transportHTTP,
				URL:      "http://127.0.0.1:8080",
				ReadOnly: tt.readOnly,
			})
			assert.Equal(t, tt.want, use)
		})
	}
}

func TestParseDaemonSyncSSEAllowsLargeDoneEvent(t *testing.T) {
	largeWarning := strings.Repeat("x", 70*1024)
	want := agentsync.SyncStats{
		TotalSessions: 12,
		Synced:        3,
		Warnings:      []string{largeWarning},
	}

	got, err := parseDaemonSyncSSE(doneSSE(t, want, true))
	require.NoError(t, err)
	assert.Equal(t, want.TotalSessions, got.TotalSessions)
	assert.Equal(t, want.Synced, got.Synced)
	require.Len(t, got.Warnings, 1)
	assert.Equal(t, largeWarning, got.Warnings[0])
}

func TestParseDaemonSyncSSEFlushesUnterminatedDoneEvent(t *testing.T) {
	want := agentsync.SyncStats{
		TotalSessions: 12,
		Synced:        3,
	}

	got, err := parseDaemonSyncSSE(doneSSE(t, want, false))
	require.NoError(t, err)
	assert.Equal(t, want.TotalSessions, got.TotalSessions)
	assert.Equal(t, want.Synced, got.Synced)
}

func TestParseDaemonSyncSSEReportsErrorEventPayload(t *testing.T) {
	_, err := parseDaemonSyncSSE(strings.NewReader(
		"event: error\n" +
			"data: remote sync failed\n" +
			"data: permission denied\n\n",
	))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon sync error")
	assert.Contains(t, err.Error(), "remote sync failed\npermission denied")
}

func TestPrintSyncProgressClearsShorterOverwrites(t *testing.T) {
	out := captureStdout(t, func() {
		printSyncProgress(agentsync.Progress{
			Detail: "Rebuilding search index",
			Hint:   "Rebuilding the search index may take a while on large archives.",
		})
		printSyncProgress(agentsync.Progress{
			Detail: "Swapping rebuilt database into place",
		})
	})

	require.GreaterOrEqual(t, strings.Count(out, "\x1b[K"), 2,
		"each carriage-return progress line must clear stale text")
	assert.Contains(t, out, "\r  Swapping rebuilt database into place\x1b[K")
}

func TestRunLocalSyncUsesCallerContextForResync(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.Exec("PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	database, err = db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	require.True(t, database.NeedsResync())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var didResync bool
	captureStdout(t, func() {
		didResync = runLocalSync(ctx, config.Config{
			DataDir: dataDir,
			DBPath:  dbPath,
		}, database, false)
	})

	assert.True(t, didResync)
	assert.True(t, database.NeedsResync())
}

func TestDoSyncUsesDaemonRouteWhenWritableDaemonRunning(t *testing.T) {
	env := newSyncCLIEnv(t)

	var syncCalled bool
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		syncCalled = true
		writeDoneSSE(t, w, agentsync.SyncStats{Synced: 7})
	})

	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	hadFailures := doSync(SyncConfig{})
	require.False(t, hadFailures)
	assert.True(t, syncCalled)
	env.assertNoLocalDB(t)
}

func TestDoSyncFullUsesDaemonResyncRoute(t *testing.T) {
	env := newSyncCLIEnv(t)

	var resyncCalled bool
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/resync", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		resyncCalled = true
		writeDoneSSE(t, w, agentsync.SyncStats{Synced: 9})
	})

	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	hadFailures := doSync(SyncConfig{Full: true})
	require.False(t, hadFailures)
	assert.True(t, resyncCalled)
	env.assertNoLocalDB(t)
}

func TestRunDaemonSyncTrimsBaseURLTrailingSlash(t *testing.T) {
	var syncCalled bool
	ts := syncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync", r.URL.Path)
		require.Equal(t, strings.TrimSuffix(tsURL(t, r), "/"), r.Header.Get("Origin"))
		syncCalled = true
		writeDoneSSE(t, w, agentsync.SyncStats{Synced: 7})
	})

	stats, err := runDaemonSync(
		context.Background(),
		transport{URL: ts.URL + "/"},
		"",
		false,
	)
	require.NoError(t, err)
	assert.True(t, syncCalled)
	assert.Equal(t, 7, stats.Synced)
}

func TestDoSyncRemoteHostUsesDaemonRouteWhenWritableDaemonRunning(t *testing.T) {
	env := newSyncCLIEnv(t)

	got, handler := captureRemoteSyncRequest(t)
	ts := remoteSyncRouteTestServer(t, handler)
	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	hadFailures := doSync(SyncConfig{
		Host: "devbox",
		User: "alice",
		Port: 2222,
		Full: true,
	})

	require.False(t, hadFailures)
	assert.False(t, got.IncludeLocal)
	assert.True(t, got.Full)
	require.Len(t, got.Hosts, 1)
	assert.Equal(t, config.RemoteHost{
		Host: "devbox",
		User: "alice",
		Port: 2222,
	}, got.Hosts[0])
	env.assertNoLocalDB(t)
}

func TestRunDaemonRemoteSyncTrimsBaseURLTrailingSlash(t *testing.T) {
	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync/remotes", r.URL.Path)
		require.Equal(t, strings.TrimSuffix(tsURL(t, r), "/"), r.Header.Get("Origin"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"failures":[]}`)
	})

	failures, err := runDaemonRemoteSync(
		context.Background(),
		transport{URL: ts.URL + "/"},
		"",
		[]config.RemoteHost{{Host: "devbox"}},
		false,
		false,
	)
	require.NoError(t, err)
	assert.Empty(t, failures)
}

func TestDoSyncConfiguredRemoteHostsUsesDaemonRouteWithLocalSync(
	t *testing.T,
) {
	env := newSyncCLIEnv(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(env.DataDir, "config.toml"),
		[]byte(`[[remote_hosts]]
host = "alpha"
user = "robot"
`),
		0o600,
	))

	got, handler := captureRemoteSyncRequest(t)
	ts := remoteSyncRouteTestServer(t, handler)
	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	hadFailures := doSync(SyncConfig{})

	require.False(t, hadFailures)
	assert.True(t, got.IncludeLocal)
	require.Len(t, got.Hosts, 1)
	assert.Equal(t, "alpha", got.Hosts[0].Host)
	assert.Equal(t, "robot", got.Hosts[0].User)
	env.assertNoLocalDB(t)
}

// syncCLIEnv is a daemon-backed CLI test environment: an isolated data dir
// exported via AGENTSVIEW_DATA_DIR with the global log writer restored on
// cleanup.
type syncCLIEnv struct {
	DataDir string
	DBPath  string
}

func newSyncCLIEnv(t *testing.T) syncCLIEnv {
	t.Helper()
	dataDir := testDataDir(t)
	restoreTestLogOutput(t)
	return syncCLIEnv{
		DataDir: dataDir,
		DBPath:  filepath.Join(dataDir, "sessions.db"),
	}
}

// assertNoLocalDB verifies the CLI deferred to the daemon instead of opening a
// local SQLite archive.
func (e syncCLIEnv) assertNoLocalDB(t *testing.T) {
	t.Helper()
	assert.NoFileExists(t, e.DBPath)
}

// remoteSyncRequest mirrors the JSON body the CLI POSTs to the daemon's
// /api/v1/sync/remotes route.
type remoteSyncRequest struct {
	Full         bool                `json:"full"`
	IncludeLocal bool                `json:"include_local"`
	Hosts        []config.RemoteHost `json:"hosts"`
}

// captureRemoteSyncRequest returns a handler that records the decoded remote
// sync request into the returned struct and replies with no failures.
func captureRemoteSyncRequest(t *testing.T) (*remoteSyncRequest, http.HandlerFunc) {
	t.Helper()
	got := &remoteSyncRequest{}
	return got, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, json.NewDecoder(r.Body).Decode(got))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"failures":[]}`)
	}
}

// doneSSE renders stats as a daemon sync "done" SSE event. When terminated is
// false the trailing blank line is omitted to exercise flush-on-EOF parsing.
func doneSSE(t *testing.T, stats agentsync.SyncStats, terminated bool) io.Reader {
	t.Helper()
	payload, err := json.Marshal(stats)
	require.NoError(t, err)
	suffix := "\n\n"
	if !terminated {
		suffix = "\n"
	}
	return strings.NewReader("event: done\ndata: " + string(payload) + suffix)
}

// writeDoneSSE writes a terminated daemon sync "done" SSE event to w.
func writeDoneSSE(t *testing.T, w io.Writer, stats agentsync.SyncStats) {
	t.Helper()
	_, err := io.Copy(w, doneSSE(t, stats, true))
	require.NoError(t, err)
}

// daemonRouteTestServer starts an httptest server that answers daemon ping
// probes and dispatches the given routes by exact path.
func daemonRouteTestServer(
	t *testing.T,
	routes map[string]http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path == "/api/ping" {
			ping.ServeHTTP(w, r)
			return
		}
		if h, ok := routes[r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func syncRouteTestServer(
	t *testing.T,
	syncHandler http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	return daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/sync":   syncHandler,
		"/api/v1/resync": syncHandler,
	})
}

func remoteSyncRouteTestServer(
	t *testing.T,
	remoteHandler http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	return daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/sync/remotes": remoteHandler,
	})
}

func registerSyncRouteTestRuntime(
	t *testing.T,
	dataDir string,
	rawURL string,
) {
	registerTestRuntime(t, dataDir, rawURL, false)
}

func registerTestRuntime(
	t *testing.T,
	dataDir string,
	rawURL string,
	readOnly bool,
) {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	_, err = WriteDaemonRuntime(dataDir, host, port, "test", readOnly)
	require.NoError(t, err)
}

func tsURL(t *testing.T, r *http.Request) string {
	t.Helper()
	return "http://" + r.Host
}
