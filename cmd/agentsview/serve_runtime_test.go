package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func heldLoopbackPort(t *testing.T) (net.Listener, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	return listener, port
}

func TestPrepareServeRuntimeConfigExplicitPortCollision(t *testing.T) {
	listener, port := heldLoopbackPort(t)
	defer listener.Close()
	_ = testDataDir(t)
	cmd := newServeCommand()
	require.NoError(t, cmd.Flags().Parse([]string{
		"--host", "127.0.0.1", "--port", strconv.Itoa(port),
	}))
	publicURL := fmt.Sprintf("https://viewer.example.test:%d/archive/", port)
	cfg := mustLoadConfig(cmd)
	cfg.PublicURL = publicURL
	cfg.PublicOrigins = []string{publicURL}

	var got config.Config
	var err error
	output := captureStdout(t, func() {
		got, err = prepareServeRuntimeConfig(cfg, serveRuntimeOptions{
			RequestedPort: port,
		})
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("requested port %d", port))
	assert.Contains(t, err.Error(), "127.0.0.1")
	assert.Equal(t, cfg, got)
	assert.NotContains(t, output, "in use, using")
	assert.Equal(t, publicURL, got.PublicURL)
	assert.Equal(t, []string{publicURL}, got.PublicOrigins)
}

func TestPrepareServeRuntimeConfigPortPolicy(t *testing.T) {
	tests := []struct {
		name  string
		fn    func(*testing.T) (config.Config, serveRuntimeOptions, func())
		check func(*testing.T, config.Config, config.Config, string)
	}{
		{
			name: "implicit collision keeps fallback and rewrites public URL",
			fn: func(t *testing.T) (config.Config, serveRuntimeOptions, func()) {
				listener, port := heldLoopbackPort(t)
				publicURL := fmt.Sprintf(
					"https://viewer.example.test:%d", port,
				)
				return config.Config{
					Host:          "127.0.0.1",
					Port:          port,
					PublicURL:     publicURL,
					PublicOrigins: []string{publicURL},
				}, serveRuntimeOptions{RequestedPort: port}, func() {
					_ = listener.Close()
				}
			},
			check: func(t *testing.T, before, after config.Config, output string) {
				assert.NotEqual(t, before.Port, after.Port)
				assert.Equal(t, fmt.Sprintf(
					"https://viewer.example.test:%d", after.Port,
				), after.PublicURL)
				assert.Equal(t, []string{after.PublicURL}, after.PublicOrigins)
				assert.Contains(t, output, "in use, using")
			},
		},
		{
			name: "explicit zero selects an ephemeral port",
			fn: func(t *testing.T) (config.Config, serveRuntimeOptions, func()) {
				return config.Config{
					Host:         "127.0.0.1",
					Port:         0,
					PortExplicit: true,
				}, serveRuntimeOptions{}, func() {}
			},
			check: func(t *testing.T, _, after config.Config, output string) {
				assert.Positive(t, after.Port)
				assert.True(t, after.PortExplicit)
				assert.Contains(t, output, "Using available port")
			},
		},
		{
			name: "free explicit port stays unchanged",
			fn: func(t *testing.T) (config.Config, serveRuntimeOptions, func()) {
				listener, port := heldLoopbackPort(t)
				listener.Close()
				return config.Config{
					Host:         "127.0.0.1",
					Port:         port,
					PortExplicit: true,
				}, serveRuntimeOptions{RequestedPort: port}, func() {}
			},
			check: func(t *testing.T, before, after config.Config, output string) {
				assert.Equal(t, before.Port, after.Port)
				assert.True(t, after.PortExplicit)
				assert.Empty(t, output)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, opts, cleanup := tt.fn(t)
			defer cleanup()
			var (
				after config.Config
				err   error
			)
			output := captureStdout(t, func() {
				after, err = prepareServeRuntimeConfig(before, opts)
			})
			require.NoError(t, err)
			tt.check(t, before, after, output)
		})
	}
}

func TestPrepareRunServeRuntimeConfigRestartPreservesConfiguredURLRewrite(t *testing.T) {
	listener, runtimePort := heldLoopbackPort(t)
	defer listener.Close()
	configuredPort := runtimePort - 1
	publicURL := fmt.Sprintf("https://viewer.example.test:%d", configuredPort)

	got, rtOpts, err := prepareRunServeRuntimeConfig(config.Config{
		Host:          "127.0.0.1",
		Port:          configuredPort,
		PublicURL:     publicURL,
		PublicOrigins: []string{publicURL},
	}, runtimePort, nil)
	require.NoError(t, err)
	assert.NotEqual(t, runtimePort, got.Port)
	assert.False(t, got.PortExplicit)
	assert.Equal(t, configuredPort, rtOpts.RequestedPort)
	assert.Equal(t, fmt.Sprintf(
		"https://viewer.example.test:%d", got.Port,
	), got.PublicURL)
	assert.Equal(t, []string{got.PublicURL}, got.PublicOrigins)

	srv := server.New(got, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	runtime, err := startServerWithOptionalCaddy(ctx, got, srv, rtOpts)
	require.NoError(t, err)
	assert.Equal(t, got.PublicURL, runtime.PublicURL)
	require.NoError(t, srv.Shutdown(context.Background()))
	require.ErrorIs(t, <-runtime.ServeErrCh, http.ErrServerClosed)
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
		server.New(config.Config{}, nil, nil), "", 300*time.Millisecond, nil,
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
		server.New(config.Config{}, nil, nil), "", 300*time.Millisecond, nil,
	)
	require.Error(t, err,
		"a listener without the server-held proof must not satisfy readiness")
}

func TestWaitForBackendReadyRejectsRedirectToServingServer(t *testing.T) {
	srv := server.New(config.Config{}, nil, nil)
	target := httptest.NewServer(srv.Handler())
	defer target.Close()
	redirector := httptest.NewServer(http.RedirectHandler(
		target.URL+"/_agentsview/startup", http.StatusTemporaryRedirect,
	))
	defer redirector.Close()

	err := waitForBackendReady(
		context.Background(), testBackendReadyConfig(redirector, ""),
		srv, "", 300*time.Millisecond, nil,
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
		srv, "", 2*time.Second, nil,
	)
	require.NoError(t, err,
		"the started server must satisfy readiness without bearer authentication")
	resp, err := http.Get(ts.URL + "/_agentsview/startup")
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
	srv := server.New(cfg, nil, nil, server.WithBasePath("/viewer/"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	runtime, err := startServerWithOptionalCaddy(
		ctx, cfg, srv, serveRuntimeOptions{Mode: "test", BasePath: "/viewer/"},
	)
	require.NoError(t, err,
		"a server mounted below a base path must satisfy backend readiness")

	require.Equal(t, fmt.Sprintf("http://127.0.0.1:%d/viewer", port), runtime.PublicURL)
	resp, err := http.Get(runtime.PublicURL + "/api/ping")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.Background(), time.Second,
	)
	defer shutdownCancel()
	require.NoError(t, srv.Shutdown(shutdownCtx))
	require.ErrorIs(t, <-runtime.ServeErrCh, http.ErrServerClosed)
}
