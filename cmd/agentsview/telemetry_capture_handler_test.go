package main

import (
	"context"
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/telemetry"
)

func TestTelemetryCanceledCaptureLeavesMigrationRunning(t *testing.T) {
	t.Setenv(telemetry.EnabledEnv, "1")
	const legacy = "install-one 2026-10-08 sessions"
	cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
	path := cfg.TelemetryScreenClaimsPath()
	require.NoError(t, os.WriteFile(path, []byte(legacy), 0o600))
	lock := flock.New(path + ".lock")
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
	forwarded := make(chan struct{}, 1)
	handler, _ := telemetryCaptureHandler(cfg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		forwarded <- struct{}{}
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/telemetry", nil))
	}()
	<-started
	select {
	case <-done:
		require.FailNow(t, "capture returned while migration was locked")
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // timer shows a live capture waits while the migration lock is held
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "canceled capture did not return while migration was locked")
	}
	assert.Empty(t, forwarded, "canceled capture must not reach the forwarding handler")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, legacy, string(data), "migration must still be waiting for the held lock")
	require.NoError(t, lock.Unlock())
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		return err == nil && jsontext.Value(data).IsValid()
	}, 5*time.Second, 10*time.Millisecond, "migration must finish after its only capture was canceled")
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"days":{"[\"install-one\",\"screen_viewed\",\"sessions\"]":["2026-10-08"]}}`, string(data))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/telemetry", nil))
	assert.Len(t, forwarded, 1, "a live capture must forward after migration completes")
}
