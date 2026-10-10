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

func TestMCPDaemonServiceRevalidatesRuntime(t *testing.T) {
	for _, change := range []string{"missing", "unknown identity", "base path", "replacement", "read-only", "fallback"} {
		t.Run(change, func(t *testing.T) {
			dir := runtimeTestDir(t)
			setTestVersion(t, "1.1.0")
			forbidStartBackgroundServeForTransport(t, "MCP must not replace a running daemon")
			cfg := config.Config{DataDir: dir, AuthToken: "test-token"}
			requests, starts := 0, 0
			server := daemonRouteTestServer(t, map[string]http.HandlerFunc{
				"/api/v1/sessions": func(w http.ResponseWriter, r *http.Request) {
					requests++
					assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
					assert.NoError(t, json.MarshalWrite(w, service.SessionList{}))
				},
				"/mounted/api/v1/sessions": func(w http.ResponseWriter, r *http.Request) {
					requests++
					assert.NoError(t, json.MarshalWrite(w, service.SessionList{Total: 9}))
				},
				"/mounted/api/ping": daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: "test"}).ServeHTTP,
			})
			host, port := splitTestServerURL(t, server.URL)
			if change == "fallback" {
				created, ok := processCreateTimeMillis(os.Getpid())
				require.True(t, ok)
				writeStartupFallbackFixture(t, dir, host, port, os.Getpid(), strconv.FormatInt(created, 10))
			} else {
				writeDaemonRuntimeForTest(t, dir, host, port, "1.1.0", change == "read-only")
			}
			if change == "unknown identity" {
				path, err := runtimeStore(dir).Path(os.Getpid())
				require.NoError(t, err)
				rec := readRuntimeRecord(t, path)
				rec.ProcessIdentityV2 = ""
				delete(rec.Metadata, runtimeCreateTime)
				_, err = runtimeStore(dir).Write(rec)
				require.NoError(t, err)
			}
			svc := newMCPDaemonService(cfg)
			_, err := svc.List(t.Context(), service.ListFilter{})
			require.NoError(t, err)
			if change == "fallback" || change == "read-only" {
				assert.Nil(t, svc.(*mcpDaemonService).backend)
				return
			}
			previous := svc.(*mcpDaemonService).backend
			require.NotNil(t, previous)
			path, err := runtimeStore(dir).Path(os.Getpid())
			require.NoError(t, err)
			rec := readRuntimeRecord(t, path)
			t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
			switch change {
			case "missing":
				t.Setenv("AGENTSVIEW_NO_DAEMON", "0")
				stubStartBackgroundServeForTransport(t, func(_ context.Context, _ *config.Config, _ time.Duration, allowReplacement bool) (*DaemonRuntime, error) {
					assert.False(t, allowReplacement, "MCP startup must not replace a daemon that appears during launch")
					starts++
					writeDaemonRuntimeForTest(t, dir, host, port, "1.1.0", false)
					return daemonRuntimeFromRecord(readRuntimeRecord(t, path)), nil
				})
				require.NoError(t, os.Remove(path))
			case "base path":
				rec.Metadata[runtimeBrowserURL] = server.URL + "/mounted"
			case "replacement":
				t.Setenv("AGENTSVIEW_NO_DAEMON", "0")
				replacement := daemonRouteTestServer(t, map[string]http.HandlerFunc{
					"/api/v1/sessions": func(w http.ResponseWriter, r *http.Request) {
						requests++
						assert.NoError(t, json.MarshalWrite(w, service.SessionList{Total: 9}))
					},
				})
				host, port = splitTestServerURL(t, replacement.URL)
				writeDaemonRuntimeForTest(t, dir, host, port, "1.0.0", false)
				rec = readRuntimeRecord(t, path)
			}
			if change != "missing" && change != "unknown identity" {
				_, err = runtimeStore(dir).Write(rec)
				require.NoError(t, err)
			}
			result, err := svc.List(t.Context(), service.ListFilter{})
			switch change {
			case "base path", "replacement":
				require.NoError(t, err)
				assert.Equal(t, 9, result.Total)
				assert.Equal(t, 2, requests)
			case "missing":
				require.NoError(t, err)
				assert.Equal(t, 1, starts)
				assert.Equal(t, 2, requests)
			case "unknown identity":
				require.NoError(t, err)
			}
			assert.NotSame(t, previous, svc.(*mcpDaemonService).backend)
		})
	}
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
	detail, err = svc.Get(t.Context(), "session-a")
	require.NoError(t, err)
	assert.Equal(t, "replacement-session", detail.ID)
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
