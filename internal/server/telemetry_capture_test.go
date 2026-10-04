package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/telemetry"
)

const telemetryEventsURL = "http://127.0.0.1:8080/api/v1/telemetry/events"

func newTelemetryCaptureServer(
	t *testing.T, requireAuth bool, opts ...Option,
) *Server {
	t.Helper()
	cfg := config.Config{Host: "127.0.0.1", Port: 8080}
	if requireAuth {
		cfg.RequireAuth, cfg.AuthToken = true, "test-token"
	}
	return New(cfg, dbtest.OpenTestDB(t), nil, opts...)
}

func postTelemetryEvent(
	t *testing.T, srv *Server, body, origin, token string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		telemetryEventsURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestTelemetryCaptureRoute(t *testing.T) {
	t.Setenv(telemetry.EnabledEnv, "0")
	reporter, err := telemetry.NewReporter(telemetry.Options{
		InstallationID: "anonymous-install-id",
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })

	authSrv := newTelemetryCaptureServer(t, true,
		WithTelemetryCapture(reporter.CaptureHandler()))
	// Bearer-authenticated requests skip the Origin check, so it is exercised without auth.
	localSrv := newTelemetryCaptureServer(t, false,
		WithTelemetryCapture(reporter.CaptureHandler()))
	const origin = "http://127.0.0.1:8080"
	oversized := `{"event":"app_opened","properties":{"pad":"` +
		strings.Repeat("x", 5000) + `"}}`

	tests := []struct {
		name     string
		srv      *Server
		body     string
		origin   string
		token    string
		wantCode int
		wantBody string
	}{
		{
			name: "no token", srv: authSrv, body: `{"event":"app_opened"}`, origin: origin,
			wantCode: http.StatusUnauthorized,
		},
		{
			name: "foreign origin", srv: localSrv, body: `{"event":"app_opened"}`,
			origin:   "http://evil.example",
			wantCode: http.StatusForbidden,
		},
		{
			name: "allowed event", srv: authSrv, body: `{"event":"app_opened"}`,
			origin: origin, token: "test-token",
			wantCode: http.StatusAccepted, wantBody: `{"status":"disabled"}`,
		},
		{
			name: "unknown event", srv: authSrv, body: `{"event":"unknown_event"}`,
			origin: origin, token: "test-token",
			wantCode: http.StatusBadRequest,
		},
		{
			name: "oversized body", srv: authSrv, body: oversized,
			origin: origin, token: "test-token",
			wantCode: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postTelemetryEvent(t, tt.srv, tt.body, tt.origin, tt.token)
			require.Equal(t, tt.wantCode, rec.Code, rec.Body.String())
			if tt.wantBody != "" {
				assert.JSONEq(t, tt.wantBody, rec.Body.String())
			}
		})
	}
}

func TestTelemetryCaptureRouteAbsentWithoutHandler(t *testing.T) {
	srv := newTelemetryCaptureServer(t, true)

	rec := postTelemetryEvent(t, srv, `{"event":"app_opened"}`,
		"http://127.0.0.1:8080", "test-token")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
}
