package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricingrefresh"
	"go.kenn.io/agentsview/internal/remotecred"
	"go.kenn.io/agentsview/internal/server"
	"hegel.dev/go/hegel"
)

func TestRemoteArchiveCommandsReadOnlyServer(t *testing.T) {
	hubDir := t.TempDir()
	hubPath := filepath.Join(hubDir, "sessions.db")
	database := seedExportSessionsArchiveAt(t, hubPath)
	require.NoError(t, pricingrefresh.SeedFallback(database))
	seedUsageDailyExportMetadataFixture(t, database, fallbackPricedModel(t))
	require.NoError(t, database.Close())
	reader, err := db.OpenReadOnly(t.Context(), hubPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	ts := httptest.NewUnstartedServer(nil)
	srv := server.New(config.Config{Host: "127.0.0.1", Port: ts.Listener.Addr().(*net.TCPAddr).Port, DataDir: hubDir, DBPath: hubPath, AuthToken: "hub-token"}, reader, nil)
	var reads atomic.Int32
	ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		if r.Header.Get("Authorization") != "Bearer hub-token" {
			http.Error(w, "wrong credential", http.StatusUnauthorized)
			return
		}
		reads.Add(1)
		srv.Handler().ServeHTTP(w, r)
	})
	ts.Start()
	t.Cleanup(ts.Close)
	localDir := t.TempDir()
	t.Setenv("HOME", localDir)
	t.Setenv("AGENTSVIEW_DATA_DIR", localDir)
	tokenPath := filepath.Join(localDir, "hub-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("hub-token\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, "config.toml"), []byte("auth_token='wrong-local-token'\n[remote]\nurl='"+ts.URL+"'\ntoken_file='"+tokenPath+"'\n[vector]\nenabled=true\n"), 0o600))
	for _, args := range [][]string{
		{"usage", "daily", "--format", "json", "--since", "2026-06-01", "--until", "2026-06-02", "--offline", "--no-sync"},
		{"usage", "statusline", "--format", "json", "--offline", "--no-sync"},
		{"activity", "report", "--format", "json", "--date", "2026-06-01", "--timezone", "UTC", "--offline", "--no-sync"},
		{"projects", "--format", "json"},
		{"health", "--format", "json"},
		{"stats", "--format", "json", "--since", "2026-06-01", "--until", "2026-06-02"},
	} {
		t.Run(args[0]+"/"+args[1], func(t *testing.T) {
			before := reads.Load()
			stdout, stderr, err := runScopedConfigCLI(t, nil, args...)
			require.NoError(t, err, stderr)
			assert.NotEmpty(t, stdout)
			assert.Greater(t, reads.Load(), before, "command must read the selected hub")
			assert.NoFileExists(t, filepath.Join(localDir, "sessions.db"))
		})
	}
}

func TestRemoteStatsCapabilityRefusesFallback(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/version", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_version":4,"read_only":true,"session_stats_available":false}`))
	}))
	t.Cleanup(ts.Close)
	_, stderr, err := runScopedConfigCLI(t, []string{"AGENTSVIEW_REMOTE_URL=" + ts.URL}, "stats", "--format", "json")
	require.Error(t, err)
	assert.Contains(t, stderr, "not available from this server")
	assert.Contains(t, stderr, "--local")
}

func TestRemoteLocalOnlyNoticePreservesOutput(t *testing.T) {
	seedScopedConfigArchive(t)
	database, err := db.Open(t.Context(), filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "sessions.db"))
	require.NoError(t, err)
	seedUsageDailyExportMetadataFixture(t, database, fallbackPricedModel(t))
	require.NoError(t, database.Close())
	for _, args := range [][]string{{"export", "sessions", "--format", "json"}, {"token-use", "usage-meta-fallback-cost"}} {
		t.Run(args[0], func(t *testing.T) {
			stdout, _, err := runScopedConfigCLI(t, nil, args...)
			require.NoError(t, err)
			remote, stderr, err := runScopedConfigCLI(t, []string{"AGENTSVIEW_REMOTE_URL=https://hub.example", "AGENTSVIEW_REMOTE_TOKEN_FILE=" + filepath.Join(t.TempDir(), "missing")}, args...)
			require.NoError(t, err, stderr)
			assert.Equal(t, stdout, remote)
			assert.Contains(t, stderr, "local archive")
		})
	}
}

func TestRemoteArchivePolicyIgnoresLocalGates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "remote-policy-token")
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", "")
	var tokens atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/usage/summary", r.URL.Path)
		assert.Equal(t, "Bearer remote-policy-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"daily":[],"totals":{"inputTokens":%d}}`, tokens.Load())
	}))
	t.Cleanup(ts.Close)
	t.Setenv("AGENTSVIEW_REMOTE_URL", ts.URL)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("auth_token='wrong-local-token'\n[vector]\nenabled=true\n"), 0o600))
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(0, math.MaxInt))
		tokens.Store(int64(count))
		policy := archiveQueryPolicy{
			Command:         targetCommand(t),
			Offline:         hegel.Draw(ht, hegel.Booleans()),
			NoSync:          hegel.Draw(ht, hegel.Booleans()),
			AutoStart:       hegel.Draw(ht, hegel.Booleans()),
			SkipInitialSync: hegel.Draw(ht, hegel.Booleans()),
			ReadOnlyDaemon:  archiveQueryReadOnlyDaemonPolicy(hegel.Draw(ht, hegel.Integers(0, 2))),
		}
		backend, cleanup, err := resolveArchiveQueryBackend(t.Context(), policy)
		require.NoError(ht, err)
		result, err := backend.DailyUsage(t.Context(), dailyUsageQuery{})
		closeArchiveQueryBackend(cleanup)
		require.NoError(ht, err)
		require.Equal(ht, count, result.Totals.InputTokens)
		require.NoFileExists(ht, filepath.Join(dir, "sessions.db"))
	}, hegel.WithTestCases(1000))
}

func TestRemoteReadGroupHelp(t *testing.T) {
	for _, name := range []string{"usage", "activity"} {
		t.Run(name, func(t *testing.T) {
			cmd := newRootCommand()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs([]string{name})
			require.NoError(t, cmd.Execute())
			assert.Contains(t, out.String(), "Available Commands:")
		})
	}
}

func TestRemoteArchiveExplicitPG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_REMOTE_URL", "https://hub.example")
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", filepath.Join(dir, "missing"))
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://reader@localhost/archive")
	database := seedExportSessionsArchiveAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	seedUsageDailyExportMetadataFixture(t, database, fallbackPricedModel(t))
	stub := stubPGReadStore(t, database)
	cmd := targetCommand(t)
	require.NoError(t, cmd.Flags().Set("pg", "true"))
	backend, cleanup, err := resolveArchiveQueryBackend(t.Context(), archiveQueryPolicy{Command: cmd, Offline: true, AutoStart: true})
	require.NoError(t, err)
	defer closeArchiveQueryBackend(cleanup)
	result, err := backend.DailyUsage(t.Context(), dailyUsageQuery{NoDefaultRange: true})
	require.NoError(t, err)
	assert.Equal(t, 300, result.Totals.InputTokens)
	report, err := backend.ActivityReport(t.Context(), ActivityReportConfig{Date: "2026-06-01", Timezone: "UTC"})
	require.NoError(t, err)
	assert.NotEmpty(t, report.BySession)
	projects, err := readProjects(cmd)
	require.NoError(t, err)
	require.NotEmpty(t, projects)
	assert.True(t, stub.Opened)
	assert.NoFileExists(t, filepath.Join(dir, "sessions.db"))
}

type remoteDeadlineTransport func(*http.Request) (*http.Response, error)

func (f remoteDeadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRemoteReportDeadline(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		long := hegel.Draw(ht, hegel.Booleans())
		callerDuration := time.Duration(hegel.Draw(ht, hegel.Integers(1, math.MaxInt64/int(time.Second)))) * time.Second
		assertRemoteReportDeadline(t, ht, long, callerDuration)
	}, hegel.WithTestCases(1000))
}

func TestRemoteReportKeeps31SecondCallerDeadline(t *testing.T) {
	assertRemoteReportDeadline(t, t, true, 31*time.Second)
}

// Reports have no total timeout; ordinary reads use 30 seconds. Both
// preserve an earlier caller deadline without waiting for a timer to fire.
func assertRemoteReportDeadline(t *testing.T, a require.TestingT, long bool, callerDuration time.Duration) {
	t.Helper()
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(callerDuration))
	defer cancel()
	callerDeadline, _ := ctx.Deadline()
	client, err := remotecred.Client("https://hub.example/private", false)
	require.NoError(a, err)
	var deadline time.Time
	client.Transport = remoteDeadlineTransport(func(r *http.Request) (*http.Response, error) {
		deadline, _ = r.Context().Deadline()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"projects":[]}`)), Request: r}, nil
	})
	timeout := 30 * time.Second
	if long {
		timeout = 0
	}
	api, err := transportAPIClient(transport{URL: "https://hub.example/private", httpClient: client}, "", &http.Client{Timeout: timeout})
	require.NoError(a, err)
	start := time.Now()
	_, err = api.GetAPIV1ProjectsWithResponse(ctx, &apiclient.GetAPIV1ProjectsRequestOptions{})
	end := time.Now()
	require.NoError(a, err)
	if long || callerDuration <= 30*time.Second {
		require.Equal(a, callerDeadline, deadline)
	} else {
		require.False(a, deadline.Before(start.Add(timeout)))
		require.False(a, deadline.After(end.Add(timeout)))
	}
}
