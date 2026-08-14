package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/daemon"
)

func testBackendReadyConfig(ts *httptest.Server, token string) config.Config {
	return config.Config{
		Host:      "127.0.0.1",
		Port:      ts.Listener.Addr().(*net.TCPAddr).Port,
		AuthToken: token,
	}
}

func TestWaitForBackendReadyRejectsUnrelatedHTTPListener(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("hello"))
		},
	))
	defer ts.Close()

	err := waitForBackendReady(
		context.Background(), testBackendReadyConfig(ts, ""),
		300*time.Millisecond, nil,
	)
	require.Error(t, err,
		"an unrelated HTTP listener must not satisfy backend readiness")
}

func TestWaitForBackendReadyRejectsDaemonPingFromAnotherProcess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(daemon.PingInfo{
				OK:      true,
				Service: daemonService,
				PID:     os.Getpid() + 1,
			})
		},
	))
	defer ts.Close()

	err := waitForBackendReady(
		context.Background(), testBackendReadyConfig(ts, ""),
		300*time.Millisecond, nil,
	)
	require.Error(t, err,
		"a daemon ping answered by another process must not satisfy readiness")
}

func TestWaitForBackendReadyAcceptsAuthenticatedDaemonPing(t *testing.T) {
	const token = "test-token"
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
	})
	mux := http.NewServeMux()
	mux.Handle("/api/ping", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ping.ServeHTTP(w, r)
		},
	))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	err := waitForBackendReady(
		context.Background(), testBackendReadyConfig(ts, token),
		2*time.Second, nil,
	)
	require.NoError(t, err,
		"an authenticated daemon ping from this process must satisfy readiness")
}
