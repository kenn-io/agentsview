package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/kit/daemon"
)

func TestNormalizeMCPHTTPAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		addr          string
		allowInsecure bool
		want          string
		wantErr       bool
	}{
		{"empty", "", false, "", true},
		{"bare port", "8085", false, "127.0.0.1:8085", false},
		{"colon port", ":8085", false, "127.0.0.1:8085", false},
		{"explicit loopback v4", "127.0.0.1:8085", false, "127.0.0.1:8085", false},
		{"explicit loopback v6", "[::1]:8085", false, "[::1]:8085", false},
		{"localhost", "localhost:8085", false, "localhost:8085", false},
		{"non-loopback rejected", "192.168.1.5:8085", false, "", true},
		{"all-interfaces rejected", "0.0.0.0:8085", false, "", true},
		{"non-loopback opted in", "192.168.1.5:8085", true, "192.168.1.5:8085", false},
		{"all-interfaces opted in", "0.0.0.0:8085", true, "0.0.0.0:8085", false},
		{"not a port", "notaport", false, "", true},
		// Empty host with a port still binds all interfaces, so it must be
		// rejected without the opt-in.
		{"empty host footgun", "[]:8085", false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeMCPHTTPAddr(tc.addr, tc.allowInsecure)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMCPListenerAuth(t *testing.T) {
	t.Parallel()
	// Loopback without require_auth is local-trust: no listener auth, even
	// when a token happens to be configured.
	tok, err := mcpListenerAuth("127.0.0.1:8085", "", false)
	require.NoError(t, err)
	assert.Empty(t, tok)
	tok, err = mcpListenerAuth("[::1]:8085", "abc", false)
	require.NoError(t, err)
	assert.Empty(t, tok, "loopback bind does not enforce a token without require_auth")

	// require_auth forces auth even on loopback, so a forwarded port is
	// never an unauthenticated surface.
	tok, err = mcpListenerAuth("127.0.0.1:8085", "abc", true)
	require.NoError(t, err)
	assert.Equal(t, "abc", tok, "require_auth enforces the token on loopback")

	// require_auth on loopback without a token is refused.
	_, err = mcpListenerAuth("127.0.0.1:8085", "", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth token")

	// Non-loopback with a token enforces it.
	tok, err = mcpListenerAuth("192.168.1.5:8085", "abc", false)
	require.NoError(t, err)
	assert.Equal(t, "abc", tok)

	// Non-loopback without a token is refused (no unauthenticated remote surface).
	_, err = mcpListenerAuth("192.168.1.5:8085", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth token")
}

func TestNewMCPCommand_Wiring(t *testing.T) {
	t.Parallel()
	cmd := newMCPCommand()
	assert.Equal(t, "mcp", cmd.Use)
	assert.Equal(t, groupData, cmd.GroupID)
	assert.True(t, cmd.SilenceUsage)

	for _, name := range []string{
		"http", "http-allow-insecure", "server", "server-token-file", "pg", "profile",
	} {
		assert.NotNil(t, cmd.Flags().Lookup(name), "missing flag --%s", name)
	}
}

func TestMCPMemoryProfileUsesPluginEnvironment(t *testing.T) {
	t.Setenv("AGENTSVIEW_MEMORY_SERVER", "https://memory.example")
	t.Setenv("AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE", "/tmp/token")
	cmd := newMCPCommand()
	require.NoError(t, cmd.ParseFlags([]string{"--profile", "memory"}))
	require.NoError(t, applyMemoryTargetEnv(cmd, "memory"))

	server, err := cmd.Flags().GetString("server")
	require.NoError(t, err)
	tokenFile, err := cmd.Flags().GetString("server-token-file")
	require.NoError(t, err)
	assert.Equal(t, "https://memory.example", server)
	assert.Equal(t, "/tmp/token", tokenFile)
}

func TestMCPFullProfileIgnoresPluginEnvironment(t *testing.T) {
	t.Setenv("AGENTSVIEW_MEMORY_SERVER", "https://memory.example")
	cmd := newMCPCommand()
	require.NoError(t, applyMemoryTargetEnv(cmd, "full"))

	server, err := cmd.Flags().GetString("server")
	require.NoError(t, err)
	assert.Empty(t, server)
}

func TestMCPMemoryProfileRejectsTokenFileWithoutServer(t *testing.T) {
	t.Setenv("AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE", "/tmp/token")
	cmd := newMCPCommand()
	require.NoError(t, cmd.ParseFlags([]string{"--profile", "memory"}))
	err := applyMemoryTargetEnv(cmd, "memory")
	require.Error(t, err)
	assert.ErrorContains(t, err, "requires --server")
}

func TestMCPMemoryProfileRejectsExplicitTokenFileWithoutServer(t *testing.T) {
	cmd := newMCPCommand()
	require.NoError(t, cmd.ParseFlags([]string{
		"--profile", "memory", "--server-token-file", "/tmp/token",
	}))
	err := applyMemoryTargetEnv(cmd, "memory")
	require.Error(t, err)
	assert.ErrorContains(t, err, "requires --server")
}

func TestMCPCommandRejectsUnknownProfileBeforeResolvingBackend(t *testing.T) {
	t.Parallel()

	_, err := executeCommand(newRootCommand(), "mcp", "--profile", "analytics")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown MCP profile")
}

func TestRootCommand_RegistersMCP(t *testing.T) {
	t.Parallel()
	root := newRootCommand()
	var found bool
	for _, c := range root.Commands() {
		if c.Use == "mcp" {
			found = true
			break
		}
	}
	assert.True(t, found, "root command should register the mcp subcommand")
}

func TestResolveMCPServicePGFlagUsesPGReadStore(t *testing.T) {
	dataDir := newAgentDataDir(t)
	remoteDir := t.TempDir()
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")
	t.Setenv("AGENTSVIEW_PG_SCHEMA", "custom_schema")
	seedSession(t, dataDir, "local-session", "local")
	seedSession(t, remoteDir, "pg-session", "remote")

	remoteDB := dbtest.OpenTestDBAt(t, filepath.Join(remoteDir, "sessions.db"))
	stub := stubPGReadStore(t, remoteDB)
	forbidStartBackgroundServeForTransport(t,
		"agentsview mcp --pg must use the PG read store, not the daemon")

	cmd := newMCPCommand()
	cmd.SetArgs([]string{"--pg"})
	require.NoError(t, cmd.ParseFlags([]string{"--pg"}))

	svc, cleanup, err := resolveMCPService(cmd)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	res, err := svc.List(t.Context(), service.ListFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res.Sessions, 1)
	assert.Equal(t, "pg-session", res.Sessions[0].ID)
	assert.Equal(t, "postgres://example.test/agentsview", stub.PG.URL)
	assert.Equal(t, "custom_schema", stub.PG.Schema)
}

func TestResolveMCPServiceExplicitServerUsesReportedCapabilities(
	t *testing.T,
) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("probe-token\n"), 0o600))

	tests := []struct {
		name                 string
		readOnly             bool
		apiVersion           int
		wantRecallCapability bool
	}{
		{
			name: "writable API v4 server", apiVersion: 4,
			wantRecallCapability: true,
		},
		{name: "writable API v3 server", apiVersion: 3},
		{name: "read-only API v4 server", readOnly: true, apiVersion: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var probeCount int
			srv := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter, r *http.Request,
			) {
				probeCount++
				assert.Equal(t, "/api/v1/version", r.URL.Path)
				assert.Equal(t, "Bearer probe-token", r.Header.Get("Authorization"))
				_ = json.MarshalWrite(w, map[string]any{
					"read_only":   tt.readOnly,
					"api_version": tt.apiVersion,
				})
			}))
			t.Cleanup(srv.Close)

			cmd := newMCPCommand()
			cmd.SetContext(t.Context())
			require.NoError(t, cmd.ParseFlags([]string{
				"--server", srv.URL,
				"--server-token-file", tokenFile,
			}))

			svc, cleanup, err := resolveMCPService(cmd)
			require.NoError(t, err)
			t.Cleanup(cleanup)

			assert.Equal(t, tt.wantRecallCapability,
				service.SupportsRecallQueries(svc))
			assert.Equal(t, 1, probeCount)
		})
	}
}

// newMCPCacheDaemon serves routes and counts ping probes. Only daemon
// discovery pings, so the count shows whether a call reused its selection.
func newMCPCacheDaemon(
	t *testing.T, routes map[string]http.HandlerFunc,
) (host string, port int, pings *atomic.Int32) {
	t.Helper()
	pings = new(atomic.Int32)
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: "test"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			pings.Add(1)
			ping.ServeHTTP(w, r)
			return
		}
		handler, ok := routes[r.URL.Path]
		if !assert.True(t, ok, "unexpected request path %s", r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	host, port = splitTestServerURL(t, server.URL)
	return host, port, pings
}

func sessionTotalHandler(t *testing.T, total int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.MarshalWrite(w, service.SessionList{Total: total}))
	}
}

func TestMCPDaemonServiceReusesVerifiedDaemon(t *testing.T) {
	dir := runtimeTestDir(t)
	forbidStartBackgroundServeForTransport(t, "MCP must reuse the running daemon")
	host, port, pings := newMCPCacheDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/sessions": func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
			sessionTotalHandler(t, 1)(w, r)
		},
	})
	writeDaemonRuntimeForTest(t, dir, host, port, "test", false)
	svc := newMCPDaemonService(config.Config{DataDir: dir, AuthToken: "test-token"})

	for range 3 {
		result, err := svc.List(t.Context(), service.ListFilter{})
		require.NoError(t, err)
		assert.Equal(t, 1, result.Total)
	}
	assert.Equal(t, int32(1), pings.Load(), "only the first call discovers the daemon")
}

func TestMCPDaemonServiceDiscoversEachCallWithoutVerifiedDaemon(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir, host string, port int)
	}{
		{
			name: "read-only daemon",
			setup: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				writeDaemonRuntimeForTest(t, dir, host, port, "test", true)
			},
		},
		{
			name: "startup fallback",
			setup: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				created, ok := processCreateTimeMillis(os.Getpid())
				require.True(t, ok)
				writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(created, 10))
			},
		},
		{
			name: "unknown process identity",
			setup: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				writeDaemonRuntimeForTest(t, dir, host, port, "test", false)
				path, err := runtimeStore(dir).Path(os.Getpid())
				require.NoError(t, err)
				rec := readRuntimeRecord(t, path)
				rec.ProcessIdentityV2 = ""
				delete(rec.Metadata, runtimeCreateTime)
				_, err = runtimeStore(dir).Write(rec)
				require.NoError(t, err)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := runtimeTestDir(t)
			forbidStartBackgroundServeForTransport(t, "MCP must use the discovered daemon")
			host, port, pings := newMCPCacheDaemon(t, map[string]http.HandlerFunc{
				"/api/v1/sessions": sessionTotalHandler(t, 1),
			})
			tt.setup(t, dir, host, port)
			svc := newMCPDaemonService(config.Config{DataDir: dir})

			_, err := svc.List(t.Context(), service.ListFilter{})
			require.NoError(t, err)
			afterFirst := pings.Load()
			require.Positive(t, afterFirst)
			_, err = svc.List(t.Context(), service.ListFilter{})
			require.NoError(t, err)
			assert.Greater(t, pings.Load(), afterFirst, "the second call must discover the daemon again")
		})
	}
}

func TestMCPDaemonServiceFollowsChangedDaemon(t *testing.T) {
	tests := []struct {
		name          string
		daemonVersion string
		exited        bool
	}{
		{name: "daemon exited and restarted", daemonVersion: "1.1.0", exited: true},
		{name: "newer daemon took over", daemonVersion: "1.2.0"},
		{name: "older daemon took over", daemonVersion: "1.0.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := runtimeTestDir(t)
			setTestVersion(t, "1.1.0")
			t.Setenv("AGENTSVIEW_NO_DAEMON", "0")
			forbidStartBackgroundServeForTransport(t, "MCP must not replace a running daemon")
			host, port, _ := newMCPCacheDaemon(t, map[string]http.HandlerFunc{
				"/api/v1/sessions": sessionTotalHandler(t, 1),
			})
			writeDaemonRuntimeForTest(t, dir, host, port, "1.1.0", false)
			svc := newMCPDaemonService(config.Config{DataDir: dir})
			result, err := svc.List(t.Context(), service.ListFilter{})
			require.NoError(t, err)
			require.Equal(t, 1, result.Total)

			nextHost, nextPort, _ := newMCPCacheDaemon(t, map[string]http.HandlerFunc{
				"/api/v1/sessions": sessionTotalHandler(t, 9),
			})
			path, err := runtimeStore(dir).Path(os.Getpid())
			require.NoError(t, err)
			starts := 0
			if tt.exited {
				require.NoError(t, os.Remove(path))
				stubStartBackgroundServeForTransport(t, func(
					_ context.Context, _ *config.Config, _ time.Duration, allowReplacement bool,
				) (*DaemonRuntime, error) {
					assert.False(t, allowReplacement, "MCP startup must not replace a daemon that appears during launch")
					starts++
					writeDaemonRuntimeForTest(t, dir, nextHost, nextPort, tt.daemonVersion, false)
					return daemonRuntimeFromRecord(readRuntimeRecord(t, path)), nil
				})
			} else {
				writeDaemonRuntimeForTest(t, dir, nextHost, nextPort, tt.daemonVersion, false)
			}

			result, err = svc.List(t.Context(), service.ListFilter{})
			require.NoError(t, err)
			assert.Equal(t, 9, result.Total, "the call must reach the daemon in the current record")
			if tt.exited {
				assert.Equal(t, 1, starts)
			}
		})
	}
}

func TestMCPDaemonServiceFailureRediscovery(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		cancelCaller   bool
		wantRediscover bool
	}{
		{name: "unknown session", status: http.StatusNotFound, body: `{"error":"not found"}`},
		{name: "source changed", status: http.StatusConflict, body: `{"error":"source_changed"}`},
		{name: "caller canceled", cancelCaller: true},
		{name: "daemon fault", status: http.StatusInternalServerError, body: `{"error":"boom"}`, wantRediscover: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := runtimeTestDir(t)
			forbidStartBackgroundServeForTransport(t, "MCP must use the running daemon")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			host, port, pings := newMCPCacheDaemon(t, map[string]http.HandlerFunc{
				"/api/v1/sessions": sessionTotalHandler(t, 1),
				"/api/v1/sessions/s1/messages": func(w http.ResponseWriter, _ *http.Request) {
					if tt.cancelCaller {
						cancel()
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.body))
				},
			})
			writeDaemonRuntimeForTest(t, dir, host, port, "test", false)
			svc := newMCPDaemonService(config.Config{DataDir: dir})
			_, err := svc.List(t.Context(), service.ListFilter{})
			require.NoError(t, err)
			require.Equal(t, int32(1), pings.Load())

			_, err = svc.Messages(ctx, "s1", service.MessageFilter{})
			require.Error(t, err)
			_, err = svc.List(t.Context(), service.ListFilter{})
			require.NoError(t, err)
			if tt.wantRediscover {
				assert.Equal(t, int32(2), pings.Load(), "a daemon fault must trigger discovery")
			} else {
				assert.Equal(t, int32(1), pings.Load(), "a serving daemon must stay selected")
			}
		})
	}
}

func TestMCPDaemonServiceFailureDoesNotWaitForDiscovery(t *testing.T) {
	dir := runtimeTestDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "0")
	failStarted, releaseFailure := make(chan struct{}), make(chan struct{})
	startEntered, releaseStart := make(chan struct{}), make(chan struct{})
	unblockFailure := sync.OnceFunc(func() { close(releaseFailure) })
	unblockStart := sync.OnceFunc(func() { close(releaseStart) })
	host, port, _ := newMCPCacheDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/sessions": sessionTotalHandler(t, 1),
		"/api/v1/sessions/s1/messages": func(w http.ResponseWriter, _ *http.Request) {
			close(failStarted)
			<-releaseFailure
			http.Error(w, "operation failed", http.StatusInternalServerError)
		},
	})
	// Registered after the server so these run before its Close waits for handlers.
	t.Cleanup(unblockStart)
	t.Cleanup(unblockFailure)
	writeDaemonRuntimeForTest(t, dir, host, port, "test", false)
	svc := newMCPDaemonService(config.Config{DataDir: dir})
	_, err := svc.List(t.Context(), service.ListFilter{})
	require.NoError(t, err)

	failed := make(chan error, 1)
	go func() {
		_, err := svc.Messages(t.Context(), "s1", service.MessageFilter{})
		failed <- err
	}()
	<-failStarted
	path, err := runtimeStore(dir).Path(os.Getpid())
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	stubStartBackgroundServeForTransport(t, func(
		context.Context, *config.Config, time.Duration, bool,
	) (*DaemonRuntime, error) {
		close(startEntered)
		<-releaseStart
		writeDaemonRuntimeForTest(t, dir, host, port, "test", false)
		return daemonRuntimeFromRecord(readRuntimeRecord(t, path)), nil
	})
	discovered := make(chan error, 1)
	go func() {
		_, err := svc.List(t.Context(), service.ListFilter{})
		discovered <- err
	}()
	select {
	case <-startEntered:
	case <-time.After(10 * time.Second):
		require.Fail(t, "removing the runtime record must make the next call discover the daemon")
	}

	unblockFailure()
	select {
	case err := <-failed:
		require.ErrorContains(t, err, "operation failed")
	case <-time.After(10 * time.Second):
		require.Fail(t, "the failed call waited for another call's daemon startup")
	}
	unblockStart()
	require.NoError(t, <-discovered)
}

func TestMCPDaemonServiceRecoversAfterCachedListenerFailure(t *testing.T) {
	dir := runtimeTestDir(t)
	forbidStartBackgroundServeForTransport(t, "MCP must use the healthy read-only daemon")
	writable := daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/sessions": func(w http.ResponseWriter, _ *http.Request) {
			assert.NoError(t, json.MarshalWrite(w, service.SessionList{Total: 1}))
		},
	})
	host, port := splitTestServerURL(t, writable.URL)
	writeDaemonRuntimeForTest(t, dir, host, port, "test", false)
	readOnlyRequests := 0
	readOnly := daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/sessions": func(w http.ResponseWriter, _ *http.Request) {
			readOnlyRequests++
			assert.NoError(t, json.MarshalWrite(w, service.SessionList{Total: 9}))
		},
	})
	readOnlyDir := t.TempDir()
	host, port = splitTestServerURL(t, readOnly.URL)
	writeDaemonRuntimeForTest(t, readOnlyDir, host, port, "test", true)
	oldList := listDaemonRuntimeRecords
	listDaemonRuntimeRecords = func(store daemon.RuntimeStore) ([]daemon.RuntimeRecord, error) {
		records, err := store.List()
		if err != nil {
			return nil, err
		}
		fallbacks, err := runtimeStore(readOnlyDir).List()
		return append(records, fallbacks...), err
	}
	t.Cleanup(func() { listDaemonRuntimeRecords = oldList })
	svc := newMCPDaemonService(config.Config{DataDir: dir})
	result, err := svc.List(t.Context(), service.ListFilter{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Total)
	writable.Close()
	result, err = svc.List(t.Context(), service.ListFilter{})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Zero(t, readOnlyRequests, "the failed operation must not be replayed")
	result, err = svc.List(t.Context(), service.ListFilter{})
	require.NoError(t, err)
	assert.Equal(t, 9, result.Total)
	assert.Equal(t, 1, readOnlyRequests)
}

func TestMCPDaemonServiceLateFailurePreservesReplacement(t *testing.T) {
	dir := runtimeTestDir(t)
	forbidStartBackgroundServeForTransport(t, "MCP must use the replacement daemon")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	writable := daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/sessions/session-a": func(w http.ResponseWriter, _ *http.Request) {
			assert.NoError(t, json.MarshalWrite(w, service.SessionDetail{ID: "session-a"}))
		},
		"/api/v1/sessions": func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			http.Error(w, "operation failed", http.StatusInternalServerError)
		},
	})
	t.Cleanup(unblock)
	host, port := splitTestServerURL(t, writable.URL)
	writeDaemonRuntimeForTest(t, dir, host, port, "test", false)
	svc := newMCPDaemonService(config.Config{DataDir: dir})
	_, err := svc.Get(t.Context(), "session-a")
	require.NoError(t, err)
	failed := make(chan error, 1)
	go func() {
		_, err := svc.List(t.Context(), service.ListFilter{})
		failed <- err
	}()
	<-started
	var probes atomic.Int32
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: "test"})
	replacement := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ping":
			probes.Add(1)
			ping.ServeHTTP(w, r)
		case "/api/v1/sessions/session-a":
			assert.NoError(t, json.MarshalWrite(w, service.SessionDetail{ID: "replacement-session"}))
		case "/api/v1/memory/status":
			assert.NoError(t, json.MarshalWrite(w, service.MemoryStatus{Status: service.MemoryReady}))
		default:
			assert.Fail(t, "unexpected request path", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(replacement.Close)
	host, port = splitTestServerURL(t, replacement.URL)
	writeDaemonRuntimeForTest(t, dir, host, port, "test", false)
	detail, err := svc.Get(t.Context(), "session-a")
	require.NoError(t, err)
	assert.Equal(t, "replacement-session", detail.ID)
	initialProbes := probes.Load()
	require.Positive(t, initialProbes)
	unblock()
	require.ErrorContains(t, <-failed, "operation failed")
	status, err := service.GetMemoryStatus(t.Context(), svc)
	require.NoError(t, err)
	assert.Equal(t, service.MemoryReady, status.Status)
	assert.Equal(t, initialProbes, probes.Load(), "the late failure must preserve the replacement cache")
}

func TestMCPDaemonServiceRejectsIncompatibleDaemonWithoutReplacement(t *testing.T) {
	for _, ahead := range []bool{false, true} {
		t.Run(fmt.Sprintf("daemon ahead %t", ahead), func(t *testing.T) {
			dir := daemonRuntimeDir(t)
			host, port := testPingServer(t)
			setTestVersion(t, "v1.1.0-3-gabcdef")
			if ahead {
				writeNewerDataVersionDaemonRuntime(t, dir, host, port, "v1.1.0-4-g123456")
			} else {
				writeIncompatibleDaemonRuntime(t, dir, host, port, "v1.1.0-2-g123456", false)
			}
			forbidStartBackgroundServeForTransport(t, "MCP must leave the incompatible daemon running")
			svc := newMCPDaemonService(config.Config{DataDir: dir})
			_, err := svc.List(t.Context(), service.ListFilter{})
			require.Error(t, err)
			assert.ErrorContains(t, err, "Restart this command with the current agentsview binary")
		})
	}
}

func TestMCPDaemonServiceRecallCapabilityFollowsResolvedRuntime(t *testing.T) {
	tests := []struct {
		name     string
		readOnly bool
		want     bool
	}{
		{name: "writable daemon", want: true},
		{name: "read-only daemon", readOnly: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := runtimeTestDir(t)
			writeLiveRuntime(t, dataDir, tt.readOnly)
			svc := newMCPDaemonService(config.Config{DataDir: dataDir})

			assert.Equal(t, tt.want, service.SupportsRecallQueries(svc))
		})
	}
}

func splitTestServerURL(t *testing.T, raw string) (string, int) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, raw, nil)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(req.URL.Host)
	require.NoError(t, err)
	var port int
	_, err = fmt.Sscanf(portText, "%d", &port)
	require.NoError(t, err)
	return host, port
}
