package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
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
		"http", "http-allow-insecure", "server", "server-token-file", "pg",
	} {
		assert.NotNil(t, cmd.Flags().Lookup(name), "missing flag --%s", name)
	}
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

	res, err := svc.List(context.Background(), service.ListFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res.Sessions, 1)
	assert.Equal(t, "pg-session", res.Sessions[0].ID)
	assert.Equal(t, "postgres://example.test/agentsview", stub.PG.URL)
	assert.Equal(t, "custom_schema", stub.PG.Schema)
}

func TestMCPDaemonServiceStartsDaemonForEachOperation(t *testing.T) {
	dataDir := t.TempDir()
	cfg := config.Config{
		DataDir: dataDir,
		DBPath:  filepath.Join(dataDir, "sessions.db"),
	}
	var starts int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/sessions", r.URL.Path)
		assert.Equal(t, "7", r.URL.Query().Get("limit"))
		_ = json.NewEncoder(w).Encode(service.SessionList{
			Sessions: []db.Session{{ID: "from-daemon", Agent: "codex"}},
			Total:    1,
		})
	}))
	t.Cleanup(ts.Close)
	host, port := splitTestServerURL(t, ts.URL)
	stubStartBackgroundServeForTransport(t, func(
		context.Context, *config.Config, time.Duration,
	) (*DaemonRuntime, error) {
		starts++
		return &DaemonRuntime{Host: host, Port: port}, nil
	})

	svc := newMCPDaemonService(cfg)
	for range 2 {
		res, err := svc.List(context.Background(), service.ListFilter{Limit: 7})
		require.NoError(t, err)
		require.Len(t, res.Sessions, 1)
		assert.Equal(t, "from-daemon", res.Sessions[0].ID)
	}
	assert.Equal(t, 2, starts)
	assert.NoFileExists(t, cfg.DBPath)
}

func splitTestServerURL(t *testing.T, raw string) (string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(req.URL.Host)
	require.NoError(t, err)
	var port int
	_, err = fmt.Sscanf(portText, "%d", &port)
	require.NoError(t, err)
	return host, port
}
