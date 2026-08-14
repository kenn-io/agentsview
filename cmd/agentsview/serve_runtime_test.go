package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/server"
)

func testBackendReadyConfig(ts *httptest.Server, token string) config.Config {
	return config.Config{
		Host:      "127.0.0.1",
		Port:      ts.Listener.Addr().(*net.TCPAddr).Port,
		AuthToken: token,
	}
}

func TestWaitForBackendReadyRejectsUnrelatedHTTPListener(t *testing.T) {
	authHeaders := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			select {
			case authHeaders <- r.Header.Get("Authorization"):
			default:
			}
			_, _ = w.Write([]byte("hello"))
		},
	))
	defer ts.Close()

	err := waitForBackendReady(
		context.Background(), testBackendReadyConfig(ts, "persistent-token"),
		server.New(config.Config{}, nil, nil), 300*time.Millisecond, nil,
	)
	require.Error(t, err,
		"an unrelated HTTP listener must not satisfy backend readiness")
	select {
	case auth := <-authHeaders:
		require.Empty(t, auth,
			"readiness must not disclose the persistent bearer token")
	case <-time.After(time.Second):
		require.Fail(t, "unrelated listener did not receive a readiness request")
	}
}

func TestWaitForBackendReadyRejectsCounterfeitStartupProof(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	))
	defer ts.Close()

	err := waitForBackendReady(
		context.Background(), testBackendReadyConfig(ts, ""),
		server.New(config.Config{}, nil, nil), 300*time.Millisecond, nil,
	)
	require.Error(t, err,
		"a listener without the server-held proof must not satisfy readiness")
}

func TestWaitForBackendReadyRejectsRedirectToServingServer(t *testing.T) {
	srv := server.New(config.Config{}, nil, nil)
	target := httptest.NewServer(srv.Handler())
	defer target.Close()
	redirector := httptest.NewServer(http.RedirectHandler(
		target.URL+srv.StartupProbePath(), http.StatusTemporaryRedirect,
	))
	defer redirector.Close()

	err := waitForBackendReady(
		context.Background(), testBackendReadyConfig(redirector, ""),
		srv, 300*time.Millisecond, nil,
	)
	require.Error(t, err,
		"a foreign first-hop listener must not relay readiness to the serving server")
}

func TestWaitForBackendReadyAcceptsAuthenticatedServerStartupProof(t *testing.T) {
	const token = "test-token"
	srv := server.New(config.Config{
		RequireAuth: true,
		AuthToken:   token,
	}, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	err := waitForBackendReady(
		context.Background(), testBackendReadyConfig(ts, token),
		srv, 2*time.Second, nil,
	)
	require.NoError(t, err,
		"the started server must satisfy readiness without bearer authentication")
	resp, err := http.Get(ts.URL + srv.StartupProbePath())
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"the temporary startup proof endpoint must be disabled after readiness")
}

func TestStartServerWithOptionalCaddyWaitsForBasePathBackend(t *testing.T) {
	port, err := server.FindAvailablePort("127.0.0.1", 0)
	require.NoError(t, err)
	cfg := config.Config{
		Host: "127.0.0.1",
		Port: port,
	}
	srv := server.New(cfg, nil, nil, server.WithBasePath("/viewer"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	runtime, err := startServerWithOptionalCaddy(
		ctx, cfg, srv, serveRuntimeOptions{Mode: "test"},
	)
	require.NoError(t, err,
		"a server mounted below a base path must satisfy backend readiness")

	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(), time.Second,
	)
	defer shutdownCancel()
	require.NoError(t, srv.Shutdown(shutdownCtx))
	require.ErrorIs(t, <-runtime.ServeErrCh, http.ErrServerClosed)
}
