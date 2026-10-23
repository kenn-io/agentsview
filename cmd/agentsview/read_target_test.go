package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/mcpdiscovery"
	"go.kenn.io/agentsview/internal/service"
	"hegel.dev/go/hegel"
)

func targetCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("server-token-file", "", "")
	cmd.Flags().Bool("pg", false, "")
	cmd.Flags().Bool("local", false, "")
	return cmd
}

func TestReadTargetSelection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	tokenPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("selected-token"), 0o600))
	hegel.Test(t, func(ht *hegel.T) {
		local := hegel.Draw(ht, hegel.Booleans())
		pg := hegel.Draw(ht, hegel.Booleans())
		server := hegel.Draw(ht, hegel.Booleans())
		remote := hegel.Draw(ht, hegel.Booleans())
		available := hegel.Draw(ht, hegel.Booleans())
		defaults := hegel.Draw(ht, hegel.Booleans())
		pgChanged := hegel.Draw(ht, hegel.Booleans())
		remoteURL, path := "", filepath.Join(dir, "missing")
		if remote {
			remoteURL = "https://hub.example"
		}
		if available {
			path = tokenPath
		}
		t.Setenv("AGENTSVIEW_REMOTE_URL", remoteURL)
		t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", path)
		cmd := targetCommand(t)
		if local {
			require.NoError(ht, cmd.Flags().Set("local", "true"))
		}
		if pg || pgChanged {
			require.NoError(ht, cmd.Flags().Set("pg", strconv.FormatBool(pg)))
		}
		if server {
			require.NoError(ht, cmd.Flags().Set("server", "http://explicit.example"))
			require.NoError(ht, cmd.Flags().Set("server-token-file", path))
		}
		target, err := resolveReadTarget(cmd, readTargetIntent(defaults))
		if server && pg || local && (server || pg) {
			require.ErrorContains(ht, err, "--")
			require.NotContains(ht, err.Error(), "token")
			return
		}
		if server && !available || !server && !pg && !local && defaults && (!remote || !available) {
			require.Error(ht, err)
			return
		}
		require.NoError(ht, err)
		want := readTargetLocal
		switch {
		case server:
			want = readTargetServer
		case pg:
			want = readTargetPG
		case !local && defaults && remote:
			want = readTargetRemote
		}
		require.Equal(ht, want, target.Kind)
		if server || want == readTargetRemote {
			require.Equal(ht, "selected-token", target.Cred)
		} else {
			require.Empty(ht, target.Cred)
		}
	}, hegel.WithTestCases(1000))
}

func TestConfiguredRemoteSurfaces(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "remote-token")
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", "")
	type observedRequest struct {
		path          string
		authorization string
	}
	requests := make(chan observedRequest, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- observedRequest{path: r.URL.Path, authorization: r.Header.Get("Authorization")}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/version" {
			_, _ = w.Write([]byte(`{"api_version":4,"read_only":true,"session_stats_available":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
	}))
	defer ts.Close()
	t.Setenv("AGENTSVIEW_REMOTE_URL", ts.URL)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "config.toml"), []byte("auth_token='local-secret'\n[vector]\nenabled=true\n[vector.embeddings]\ndimension=-1\n"), 0o600))
	cmd := targetCommand(t)
	target, err := resolveReadTarget(cmd, readTargetDefault)
	require.NoError(t, err)
	require.Equal(t, readTargetRemote, target.Kind)
	caps, err := target.probe(t.Context())
	require.NoError(t, err)
	require.True(t, caps.SessionStatsAvailable)
	svc, cleanup, err := resolveService(cmd)
	require.NoError(t, err)
	defer cleanup()
	_, err = svc.List(t.Context(), service.ListFilter{})
	require.NoError(t, err)
	require.Len(t, requests, 2)
	require.Equal(t, []observedRequest{
		{path: "/api/v1/version", authorization: "Bearer remote-token"},
		{path: "/api/v1/sessions", authorization: "Bearer remote-token"},
	}, []observedRequest{<-requests, <-requests})
	_, err = resolveCodebuffBareID(cmd, svc, "2026-07-16T00-09-00.236Z")
	require.ErrorContains(t, err, "configured [remote]")
	require.ErrorContains(t, err, "--local")
}

func TestRemoteMemoryModes(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_REMOTE_URL", "https://hub.example")
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	original := runMemorySessionStart
	defer func() { runMemorySessionStart = original }()
	runMemorySessionStart = func(_ context.Context, req memorySessionStartRequest) error { require.Empty(t, req.Server); return nil }
	for _, mode := range []string{"local", "hosted-contributor", "hosted-reader"} {
		cmd := newMemorySessionStartCommand()
		cmd.SetContext(t.Context())
		require.NoError(t, cmd.Flags().Set("mode", mode))
		if mode == "hosted-contributor" {
			require.NoError(t, cmd.Flags().Set("target", "replica"))
		}
		err := runMemorySessionStartCommand(cmd, "")
		if mode == "hosted-reader" {
			require.ErrorContains(t, err, "remote token")
		} else {
			require.NoError(t, err)
		}
	}
}

func TestRemoteReadAdapters(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", "")
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "remote-token")
	type observedRequest struct {
		method        string
		path          string
		authorization string
	}
	requests := make(chan observedRequest, 32)
	handlerErrors := make(chan string, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- observedRequest{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
		}
		if r.Method != http.MethodGet {
			handlerErrors <- fmt.Sprintf("unexpected method %s for %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"api_version":6,"read_only":true}`))
		case "/api/v1/recall/entries":
			_, _ = w.Write([]byte(`{"entries":[{"id":"remote-recall","type":"procedure","scope":"project","status":"accepted","title":"Remote recall","project":"agentsview"}],"trusted_only":false}`))
		case "/api/v1/sessions":
			_, _ = w.Write([]byte(`{"sessions":[{"id":"mcp-remote-session","agent":"codex","project":"agentsview"}],"total":1}`))
		case "/api/v1/sessions/claude:test":
			_, _ = w.Write([]byte(`{"id":"claude:test"}`))
		case "/api/v1/sessions/claude:test/usage":
			_, _ = w.Write([]byte(`{"total_output_tokens":17,"has_token_data":true}`))
		default:
			handlerErrors <- fmt.Sprintf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer ts.Close()
	t.Setenv("AGENTSVIEW_REMOTE_URL", ts.URL)
	cmd := targetCommand(t)
	recallService, cleanup, err := resolveRecallEntryService(cmd)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	recallList, err := recallService.ListRecallEntries(t.Context(), service.RecallFilter{Project: "agentsview"})
	require.NoError(t, err)
	require.Len(t, recallList.RecallEntries, 1)
	require.Equal(t, "remote-recall", recallList.RecallEntries[0].ID)
	require.Len(t, requests, 1)
	require.Equal(t, observedRequest{
		method: http.MethodGet, path: "/api/v1/recall/entries", authorization: "Bearer remote-token",
	}, <-requests)

	mcpService, cleanup, err := resolveMCPService(cmd)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	require.Len(t, requests, 1)
	require.Equal(t, observedRequest{
		method: http.MethodGet, path: "/api/v1/version", authorization: "Bearer remote-token",
	}, <-requests)
	sessions, err := mcpService.List(t.Context(), service.ListFilter{Project: "agentsview"})
	require.NoError(t, err)
	require.Len(t, sessions.Sessions, 1)
	require.Equal(t, "mcp-remote-session", sessions.Sessions[0].ID)
	require.Len(t, requests, 1)
	require.Equal(t, observedRequest{
		method: http.MethodGet, path: "/api/v1/sessions", authorization: "Bearer remote-token",
	}, <-requests)

	tr, _, err := resolveInsightTransport(cmd)
	require.NoError(t, err)
	require.Equal(t, ts.URL, tr.URL)
	cmd.Flags().Bool("own-only", false, "")
	cmd.Flags().Bool("no-sync", false, "")
	for _, ownOnly := range []bool{false, true} {
		for _, noSync := range []bool{false, true} {
			require.NoError(t, cmd.Flags().Set("own-only", strconv.FormatBool(ownOnly)))
			require.NoError(t, cmd.Flags().Set("no-sync", strconv.FormatBool(noSync)))
			out, code, err := sessionUsageDataForCommand(cmd, "claude:test")
			require.NoError(t, err)
			require.Equal(t, 0, code)
			require.Equal(t, 17, out.TotalOutputTokens)
		}
	}
	var unexpectedRequests []string
	for len(handlerErrors) > 0 {
		unexpectedRequests = append(unexpectedRequests, <-handlerErrors)
	}
	require.Empty(t, unexpectedRequests)
}

func TestLocalOnlyReadersIgnoreRemote(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	t.Setenv("AGENTSVIEW_REMOTE_URL", "https://hub.example")
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	for _, args := range [][]string{{"secrets", "list"}, {"session", "watch", "missing"}, {"recall", "extract", "preview", "--session", "missing"}} {
		_, err := executeCommand(newRootCommand(), args...)
		if err != nil {
			require.NotContains(t, err.Error(), "remote token", args)
		}
	}
}

func TestRemoteProbeDiagnostics(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", "")
	for _, body := range []string{`null`, `{}`, `{"api_version":1}`} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		t.Setenv("AGENTSVIEW_REMOTE_URL", ts.URL+"/private")
		cmd := targetCommand(t)
		var err error
		if body == `{"api_version":1}` {
			cmd.Flags().Bool("own-only", false, "")
			cmd.Flags().Bool("no-sync", false, "")
			_, _, err = sessionUsageDataForCommand(cmd, "claude:test")
		} else {
			target, e := resolveReadTarget(cmd, readTargetDefault)
			require.NoError(t, e)
			_, err = target.probe(t.Context())
		}
		require.Error(t, err)
		require.Contains(t, err.Error(), ts.URL)
		require.Contains(t, err.Error(), "--local")
		require.NotContains(t, err.Error(), "/private")
		ts.Close()
	}
}

func TestMemoryRemotePrecedence(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_REMOTE_URL", "https://hub.example")
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("AGENTSVIEW_MEMORY_SERVER", "https://memory.example")
	t.Setenv("AGENTSVIEW_MEMORY_MODE", "hosted-reader")
	for _, tc := range []struct {
		profile, flag, value string
		kind                 readTargetKind
		fails                bool
	}{
		{"memory", "", "", readTargetServer, false},
		{"memory", "pg", "false", "", true},
		{"memory", "local", "true", readTargetLocal, false},
		{"default", "", "", "", true},
	} {
		cmd := targetCommand(t)
		if tc.flag != "" {
			require.NoError(t, cmd.Flags().Set(tc.flag, tc.value))
		}
		require.NoError(t, applyMemoryTargetEnv(cmd, tc.profile))
		target, err := resolveReadTarget(cmd, readTargetDefault)
		if tc.fails {
			require.ErrorContains(t, err, "remote token")
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.kind, target.Kind)
		}
	}
	original := runMemorySessionStart
	defer func() { runMemorySessionStart = original }()
	var got memorySessionStartRequest
	runMemorySessionStart = func(_ context.Context, req memorySessionStartRequest) error { got = req; return nil }
	cmd := newMemorySessionStartCommand()
	cmd.SetContext(t.Context())
	require.NoError(t, runMemorySessionStartCommand(cmd, ""))
	require.Equal(t, "https://memory.example", got.Server)
	t.Setenv("AGENTSVIEW_MEMORY_SERVER", "")
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("remote-only"), 0o600))
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", path)
	cmd = newMemorySessionStartCommand()
	cmd.SetContext(t.Context())
	require.NoError(t, runMemorySessionStartCommand(cmd, ""))
	require.Equal(t, "https://hub.example", got.Server)
	require.Equal(t, "remote-only", got.ServerToken)
	require.NotNil(t, got.HTTPClient)
}

func TestRemoteMCPDiscovery(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", "")
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_version":6,"read_only":true}`))
	}))
	defer hub.Close()
	t.Setenv("AGENTSVIEW_REMOTE_URL", hub.URL)
	cmd := newMCPCommand()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd.SetContext(ctx)
	require.NoError(t, cmd.Flags().Set("http", "127.0.0.1:0"))
	done := make(chan error, 1)
	go func() { done <- cmd.RunE(cmd, nil) }()
	require.Eventually(t, func() bool {
		rows, err := mcpdiscovery.List(filepath.Join(dir, "mcp"))
		return err == nil && len(rows) == 1 && rows[0].BackendURL == hub.URL
	}, 15*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
}

func TestRemoteUsageRedactsUnexpectedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"partial content", "private-path?token=response-secret", http.StatusPartialContent},
		{"invalid success", `{"response-secret":1,"response-secret":2}`, http.StatusOK},
		{"deleted after lookup", "deleted", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
			t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", "")
			usageRequests := make(chan struct{}, 1)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/version":
					_, _ = w.Write([]byte(`{"api_version":6,"read_only":true}`))
				case "/api/v1/sessions/claude:test":
					_, _ = w.Write([]byte(`{"id":"claude:test"}`))
				case "/api/v1/sessions/claude:test/usage":
					usageRequests <- struct{}{}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				default:
					http.Error(w, "unexpected request", http.StatusInternalServerError)
				}
			}))
			t.Cleanup(ts.Close)
			t.Setenv("AGENTSVIEW_REMOTE_URL", ts.URL)
			out, code, err := sessionUsageDataForCommand(targetCommand(t), "claude:test")
			require.Len(t, usageRequests, 1)
			if tc.status == http.StatusNotFound {
				require.NoError(t, err)
				require.Nil(t, out)
				require.Equal(t, tokenUseExitNotFound, code)
				return
			}
			require.Error(t, err)
			require.NotContains(t, err.Error(), "response-secret")
			require.NotContains(t, err.Error(), "private-path")
			require.Contains(t, err.Error(), "--local")
		})
	}
}
