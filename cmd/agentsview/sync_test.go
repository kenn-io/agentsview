package main

import (
	"bytes"
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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/remotesync"
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

func TestRunRemoteSyncOnceDispatchesHTTP(t *testing.T) {
	var called config.RemoteHost
	restore := stubHTTPRemoteSyncForTest(t, func(
		_ context.Context,
		rh config.RemoteHost,
		full bool,
	) (remotesync.SyncStats, error) {
		called = rh
		assert.True(t, full)
		return remotesync.SyncStats{SessionsSynced: 2}, nil
	})
	defer restore()

	err := runRemoteSyncOnce(config.Config{}, nil, config.RemoteHost{
		Host: "devbox", Transport: config.RemoteTransportHTTP,
		URL: "http://devbox:8080", Token: "remote-token",
	}, true)

	require.NoError(t, err)
	assert.Equal(t, "http://devbox:8080", called.URL)
}

func TestRunRemoteSyncOnceWithRepairDispatchesHTTPRepairDirectly(t *testing.T) {
	var called config.RemoteHost
	var gotFull, gotRepair bool
	restore := stubHTTPRemoteSyncWithRepairForTest(t, func(
		_ context.Context,
		rh config.RemoteHost,
		full, repairMirror bool,
	) (remotesync.SyncStats, error) {
		called = rh
		gotFull = full
		gotRepair = repairMirror
		return remotesync.SyncStats{SessionsSynced: 2}, nil
	})
	defer restore()

	err := runRemoteSyncOnceWithRepair(config.Config{}, nil, config.RemoteHost{
		Host: "devbox", Transport: config.RemoteTransportHTTP,
		URL: "http://devbox:8080", Token: "remote-token",
	}, true, true)

	require.NoError(t, err)
	assert.Equal(t, "http://devbox:8080", called.URL)
	assert.True(t, gotFull)
	assert.True(t, gotRepair)
}

func TestRunHTTPRemoteSyncRequiresExplicitHTTPToken(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(ts.Close)

	_, err := runHTTPRemoteSync(
		context.Background(),
		config.Config{AuthToken: "collector-token"},
		nil,
		config.RemoteHost{
			Host:      "devbox",
			Transport: config.RemoteTransportHTTP,
			URL:       ts.URL,
		},
		false,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "token is required")
	assert.False(t, called, "collector auth_token must not be sent to remote")
}

func stubHTTPRemoteSyncForTest(
	t *testing.T,
	fn func(context.Context, config.RemoteHost, bool) (remotesync.SyncStats, error),
) func() {
	t.Helper()
	orig := runHTTPRemoteSync
	runHTTPRemoteSync = func(
		ctx context.Context,
		_ config.Config,
		_ *db.DB,
		rh config.RemoteHost,
		full bool,
	) (remotesync.SyncStats, error) {
		return fn(ctx, rh, full)
	}
	return func() { runHTTPRemoteSync = orig }
}

func stubHTTPRemoteSyncWithRepairForTest(
	t *testing.T,
	fn func(context.Context, config.RemoteHost, bool, bool) (remotesync.SyncStats, error),
) func() {
	t.Helper()
	orig := runHTTPRemoteSyncWithRepair
	runHTTPRemoteSyncWithRepair = func(
		ctx context.Context,
		_ config.Config,
		_ *db.DB,
		rh config.RemoteHost,
		full, repairMirror bool,
	) (remotesync.SyncStats, error) {
		return fn(ctx, rh, full, repairMirror)
	}
	return func() { runHTTPRemoteSyncWithRepair = orig }
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

func TestParseDaemonSyncSSEReportsProgressEvents(t *testing.T) {
	want := agentsync.SyncStats{
		TotalSessions: 12,
		Synced:        3,
	}
	var progress []agentsync.Progress

	got, err := parseDaemonSyncSSE(strings.NewReader(
		"event: progress\n"+
			"data: {\"phase\":\"rebuilding_search\",\"detail\":\"Rebuilding search index\",\"resync\":true}\n\n"+
			sseString(t, doneSSE(t, want, true)),
	), func(p agentsync.Progress) {
		progress = append(progress, p)
	})

	require.NoError(t, err)
	assert.Equal(t, want.Synced, got.Synced)
	require.Len(t, progress, 1)
	assert.Equal(t, agentsync.PhaseRebuildingSearch, progress[0].Phase)
	assert.Equal(t, "Rebuilding search index", progress[0].Detail)
	assert.True(t, progress[0].Resync)
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

func TestResyncProgressPrinterWritesPhaseTimingsOnNewLines(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newResyncProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Phase:  agentsync.PhasePreparingResync,
		Detail: "Preparing full resync",
		Resync: true,
	})
	now = now.Add(150 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   10,
		SessionsDone:    4,
		MessagesIndexed: 40,
		Resync:          true,
	})
	now = now.Add(2 * time.Second)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   10,
		SessionsDone:    10,
		MessagesIndexed: 100,
		Resync:          true,
	})
	now = now.Add(350 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:  agentsync.PhaseRebuildingSearch,
		Detail: "Rebuilding search index",
		Hint:   "Rebuilding the search index may take a while on large archives.",
		Resync: true,
	})
	now = now.Add(3 * time.Second)
	printer.Print(agentsync.Progress{
		Phase:  agentsync.PhaseSwappingDatabase,
		Detail: "Swapping rebuilt database into place",
		Resync: true,
	})

	got := out.String()
	assert.Contains(t, got, "  Preparing full resync...\n")
	assert.Contains(t, got, "  Preparing full resync completed in 150ms\n")
	assert.Contains(t, got, "\r  Syncing sessions into rebuilt database: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.Contains(t, got, "\n  Syncing sessions into rebuilt database completed in 2.35s\n")
	assert.Contains(t, got, "  Rebuilding search index - Rebuilding the search index may take a while on large archives...\n")
	assert.Contains(t, got, "  Rebuilding search index completed in 3s\n")
	assert.NotContains(t, got, "\r  Rebuilding search index",
		"non-session resync phases must not be overwritten in place")
}

func TestResyncProgressPrinterRendersDoneProgressBeforeCompletion(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newResyncProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   1,
		SessionsDone:    1,
		MessagesIndexed: 0,
		Resync:          true,
	})
	now = now.Add(time.Second)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseDone,
		SessionsTotal:   1,
		SessionsDone:    1,
		MessagesIndexed: 42,
		Resync:          true,
	})

	got := out.String()
	assert.Contains(t, got,
		"\r  Syncing sessions into rebuilt database: 1/1 sessions (100%) · 42 messages\x1b[K")
	assert.Contains(t, got,
		"\n  Syncing sessions into rebuilt database completed in 1s\n")
}

func TestRemoteProgressPrinterRendersMirrorPhasesAndThroughput(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newRemoteProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Detail: "Resolving agent directories on devbox",
	})
	now = now.Add(150 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Detail: "Downloading session data from devbox (3 agents)",
	})
	now = now.Add(2 * time.Second)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Processing sessions from devbox",
		SessionsTotal:   10,
		SessionsDone:    4,
		MessagesIndexed: 40,
	})
	now = now.Add(350 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Processing sessions from devbox",
		SessionsTotal:   10,
		SessionsDone:    10,
		MessagesIndexed: 100,
	})
	now = now.Add(3 * time.Second)
	printer.Print(agentsync.Progress{
		Detail: "Synced 10 sessions from devbox (1 unchanged)",
	})
	printer.Finish()

	got := out.String()
	assert.Contains(t, got, "  Resolving agent directories on devbox...\n")
	assert.Contains(t, got, "  Resolving agent directories on devbox completed in 150ms\n")
	assert.Contains(t, got, "  Downloading session data from devbox (3 agents)...\n")
	assert.Contains(t, got, "  Downloading session data from devbox (3 agents) completed in 2s\n")
	assert.Contains(t, got, "\r  Processing sessions from devbox: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.Contains(t, got, "\n  Processing sessions from devbox completed in 3.35s, 3.0 sessions/s\n")
	assert.Contains(t, got, "  Synced 10 sessions from devbox (1 unchanged)\n")
	assert.True(t, strings.HasSuffix(got, "\n"), "remote progress should finish on a newline")
}

func TestRemoteProgressPrinterRendersByteProgressInPlace(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newRemoteProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Detail:     "Downloading session archive from devbox",
		BytesDone:  1 << 20,
		BytesTotal: 4 << 20,
	})
	now = now.Add(150 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Detail:     "Downloading session archive from devbox",
		BytesDone:  4 << 20,
		BytesTotal: 4 << 20,
	})
	now = now.Add(850 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Detail: "Extracting session archive from devbox",
	})
	printer.Finish()

	got := out.String()
	assert.Contains(t, got,
		"\r  Downloading session archive from devbox: 1.0 MB/4.0 MB (25%)\x1b[K")
	assert.Contains(t, got,
		"\r  Downloading session archive from devbox: 4.0 MB/4.0 MB (100%)\x1b[K")
	assert.Contains(t, got,
		"\n  Downloading session archive from devbox completed in 1s\n")
	assert.Contains(t, got, "  Extracting session archive from devbox...\n")
}

func TestRemoteProgressPrinterRendersLocalSyncProgressWithoutDetail(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newRemoteProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		SessionsTotal:   10,
		SessionsDone:    4,
		MessagesIndexed: 40,
	})
	now = now.Add(250 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseDone,
		SessionsTotal:   10,
		SessionsDone:    10,
		MessagesIndexed: 100,
	})
	printer.Print(agentsync.Progress{
		Detail: "Resolving agent directories on devbox",
	})

	got := out.String()
	assert.Contains(t, got, "\r  Syncing local sessions: 4/10 sessions (40%) · 40 messages\x1b[K")
	assert.Contains(t, got, "\r  Syncing local sessions: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.Contains(t, got, "\n  Syncing local sessions completed in 250ms\n")
	assert.Contains(t, got, "  Resolving agent directories on devbox...\n")
}

func TestRemoteProgressPrinterKeepsResyncLabelOnDoneProgress(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	var out bytes.Buffer
	printer := newRemoteProgressPrinter(&out, clock)

	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseSyncing,
		Detail:          "Syncing sessions into rebuilt database",
		SessionsTotal:   10,
		SessionsDone:    4,
		MessagesIndexed: 40,
		Resync:          true,
	})
	now = now.Add(250 * time.Millisecond)
	printer.Print(agentsync.Progress{
		Phase:           agentsync.PhaseDone,
		SessionsTotal:   10,
		SessionsDone:    10,
		MessagesIndexed: 100,
		Resync:          true,
	})

	got := out.String()
	assert.Contains(t, got, "\r  Syncing sessions into rebuilt database: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.NotContains(t, got, "\r  Syncing local sessions: 10/10 sessions (100%) · 100 messages\x1b[K")
	assert.Contains(t, got, "\n  Syncing sessions into rebuilt database completed in 250ms\n")
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
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"event: progress\n"+
				"data: {\"phase\":\"rebuilding_search\",\"detail\":\"Rebuilding search index\",\"resync\":true}\n\n",
		)
		require.NoError(t, err)
		writeDoneSSE(t, w, agentsync.SyncStats{Synced: 9})
	})

	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	var hadFailures bool
	out := captureStdout(t, func() {
		hadFailures = doSync(SyncConfig{Full: true})
	})
	require.False(t, hadFailures)
	assert.True(t, resyncCalled)
	assert.Contains(t, out, "Rebuilding search index")
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
		nil,
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
		Host:         "devbox",
		User:         "alice",
		Port:         2222,
		Full:         true,
		RepairMirror: true,
	})

	require.False(t, hadFailures)
	assert.False(t, got.IncludeLocal)
	assert.True(t, got.Full)
	assert.True(t, got.RepairMirror)
	require.Len(t, got.Hosts, 1)
	assert.Equal(t, config.RemoteHost{
		Host: "devbox",
		User: "alice",
		Port: 2222,
	}, got.Hosts[0])
	env.assertNoLocalDB(t)
}

func TestDoSyncRemoteHostPrintsDaemonProgress(t *testing.T) {
	env := newSyncCLIEnv(t)

	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync/remotes", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"event: progress\n"+
				"data: {\"detail\":\"Resolving agent directories on devbox\"}\n\n"+
				"event: done\n"+
				"data: {\"failures\":[]}\n\n",
		)
		require.NoError(t, err)
	})
	registerSyncRouteTestRuntime(t, env.DataDir, ts.URL)

	var hadFailures bool
	out := captureStdout(t, func() {
		hadFailures = doSync(SyncConfig{Host: "devbox"})
	})

	require.False(t, hadFailures)
	assert.Contains(t, out, "Running sync with remotes via daemon...")
	assert.Contains(t, out, "Resolving agent directories on devbox")
	assert.True(t, strings.HasSuffix(out, "\n"), "progress output should finish on a newline")
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
		nil,
	)
	require.NoError(t, err)
	assert.Empty(t, failures)
}

func TestRunDaemonRemoteSyncReportsProgressEvents(t *testing.T) {
	ts := remoteSyncRouteTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/sync/remotes", r.URL.Path)
		assert.Contains(t, r.Header.Get("Accept"), "text/event-stream")
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w,
			"event: progress\n"+
				"data: {\"phase\":\"rebuilding_search\",\"detail\":\"Rebuilding search index\",\"resync\":true}\n\n"+
				"event: done\n"+
				"data: {\"failures\":[]}\n\n",
		)
		require.NoError(t, err)
	})
	var progress []agentsync.Progress

	failures, err := runDaemonRemoteSync(
		context.Background(),
		transport{URL: ts.URL},
		"",
		[]config.RemoteHost{{Host: "devbox"}},
		false,
		true,
		func(p agentsync.Progress) {
			progress = append(progress, p)
		},
	)

	require.NoError(t, err)
	assert.Empty(t, failures)
	require.Len(t, progress, 1)
	assert.Equal(t, agentsync.PhaseRebuildingSearch, progress[0].Phase)
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
	RepairMirror bool                `json:"repair_mirror"`
	IncludeLocal bool                `json:"include_local"`
	Hosts        []config.RemoteHost `json:"hosts"`
}

func TestRemoteSyncRepairIntentContract(t *testing.T) {
	body, err := json.Marshal(remoteSyncRequest{Full: true, RepairMirror: true})
	require.NoError(t, err)
	assert.JSONEq(t, `{"full":true,"repair_mirror":true,"include_local":false,"hosts":null}`, string(body))
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

func sseString(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
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

func TestRemoteFailureDisplaySanitizesHTTPErrors(t *testing.T) {
	tests := []struct {
		name       string
		failure    remoteHostFailure
		want       string
		wantAbsent []string
	}{
		{
			name: "http status failure uses sanitized summary",
			failure: remoteHostFailure{
				Host: config.RemoteHost{
					Host:      "devbox",
					Transport: config.RemoteTransportHTTP,
				},
				Err: &remotesync.StatusError{
					Code:   401,
					Status: "401 Unauthorized",
					Detail: "bearer secret-token-123 rejected",
				},
			},
			want: "HTTP remote sync failed: remote daemon rejected " +
				"the sync token (401 Unauthorized); the token for " +
				"this host in [[remote_hosts]] must match the remote " +
				"daemon's auth_token",
			wantAbsent: []string{"secret-token-123"},
		},
		{
			name: "http transport collapses unknown raw errors",
			failure: remoteHostFailure{
				Host: config.RemoteHost{
					Host:      "devbox",
					Transport: config.RemoteTransportHTTP,
				},
				Err: errors.New(
					`Get "http://devbox.tailnet.ts.net:8080": token=abc rejected`,
				),
			},
			want:       "HTTP remote sync failed",
			wantAbsent: []string{"tailnet.ts.net", "token=abc"},
		},
		{
			name: "ssh transport keeps the raw error",
			failure: remoteHostFailure{
				Host: config.RemoteHost{Host: "buildbox"},
				Err:  errors.New("ssh: permission denied"),
			},
			want: "ssh: permission denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := remoteFailureDisplay(tt.failure)
			assert.Equal(t, tt.want, got)
			for _, absent := range tt.wantAbsent {
				assert.NotContains(t, got, absent)
			}
		})
	}
}

func TestRunHTTPRemoteSyncReachesMirrorPath(t *testing.T) {
	manifestRequests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case "/api/v1/remote-sync/manifest":
			manifestRequests++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	database := dbtest.OpenTestDB(t)

	_, err := runHTTPRemoteSync(
		context.Background(),
		config.Config{DataDir: t.TempDir()},
		database,
		config.RemoteHost{
			Host:      "devbox",
			Transport: config.RemoteTransportHTTP,
			URL:       ts.URL,
			Token:     "remote-token",
		},
		false,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, manifestRequests,
		"configured DataDir must route HTTP sync through the manifest/mirror path")
}
