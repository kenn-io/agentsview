package telemetry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kittelemetry "go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
)

func TestCoreActionAllowlist(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	endpoint, captured := captureCollector(t)
	reporter := captureReporter(t, endpoint, Options{AgentTypes: []string{"freebuff"}, InsightKinds: []string{"daily_activity"}})
	srv := server.New(config.Config{Host: "127.0.0.1", Port: 8080}, dbtest.OpenTestDB(t), nil, server.WithTelemetryCapture(reporter.CaptureHandler()))
	cases := []struct {
		event, key, value string
		kept              bool
	}{
		{EventSearchRun, "query_type", "semantic", true},
		{EventSearchRun, "query_type", "regex", false},
		{EventSessionViewed, "agent", "freebuff", true},
		{EventSessionViewed, "agent", "/Users/alice/secret", false},
		{EventExportRun, "format", "markdown_link", true},
		{EventExportRun, "format", "pdf", false},
		{EventInsightGenerated, "kind", "daily_activity", true},
		{EventInsightGenerated, "kind", "llm_canned", false},
		{EventAnalyticsViewed, "page", "trends", true},
		{EventAnalyticsViewed, "page", "sessions", false},
		{EventScreenViewed, "screen", "activity", true},
		{EventScreenViewed, "screen", "trends", true},
		{EventScreenViewed, "screen", "recall", true},
		{EventScreenViewed, "screen", "quality", true},
		{EventScreenViewed, "screen", "pinned", true},
		{EventScreenViewed, "screen", "trash", true},
		{EventScreenViewed, "screen", "recent-edits", true},
		{EventScreenViewed, "screen", "data", true},
		{EventScreenViewed, "screen", "settings", true},
		{EventScreenViewed, "screen", "unknown", false},
		{EventScreenViewed, "surface", "terminal", false},
		{EventVisitEnded, "duration_bucket", "under_1m", true},
		{EventVisitEnded, "duration_bucket", "1_to_5m", true},
		{EventVisitEnded, "duration_bucket", "5_to_30m", true},
		{EventVisitEnded, "duration_bucket", "over_30m", true},
		{EventVisitEnded, "duration_bucket", "120s", false},
		{EventVisitEnded, "surface", "web", true},
		{EventVisitEnded, "surface", "terminal", false},
		// A later visit with the same bucket must reach the collector again.
		{EventVisitEnded, "duration_bucket", "1_to_5m", true},
	}
	for _, c := range cases {
		properties := map[string]any{c.key: c.value, "query": "secret prompt"}
		if c.event == EventVisitEnded {
			properties["duration_ms"] = 120000
			if c.key == "duration_bucket" {
				properties["surface"] = "web"
			}
		}
		if c.event == EventScreenViewed && c.key == "surface" {
			properties["screen"] = "sessions"
		}
		body, err := json.Marshal(map[string]any{"event": c.event, "properties": properties})
		require.NoError(t, err)
		status := http.StatusAccepted
		if c.event == EventScreenViewed && c.key == "screen" && !c.kept {
			status = http.StatusBadRequest
		}
		postCapture(t, srv.Handler(), string(body), status)
	}
	require.NoError(t, reporter.Close())
	sent := captured()
	require.Len(t, sent, len(cases)-1)
	i := 0
	for _, c := range cases {
		if c.event == EventScreenViewed && c.key == "screen" && !c.kept {
			continue
		}
		assert.NotContains(t, sent[i], "query", c.event)
		if c.event == EventVisitEnded {
			assert.NotContains(t, sent[i], "duration_ms")
			if c.key == "duration_bucket" {
				assert.Equal(t, "web", sent[i]["surface"])
			}
		}
		if c.kept {
			assert.Equal(t, c.value, sent[i][c.key])
		} else {
			assert.NotContains(t, sent[i], c.key)
		}
		i++
	}
}

func TestScreenRequestValidationLeavesValidRetry(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	const valid = `{"event":"screen_viewed","properties":{"screen":"sessions"}}`
	for _, tc := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"missing content type", "", valid, http.StatusUnsupportedMediaType},
		{"plain text", "text/plain", valid, http.StatusUnsupportedMediaType},
		{"trailing value", "application/json", valid + " {}", http.StatusBadRequest},
		{"trailing garbage", "application/json", valid + " x", http.StatusBadRequest},
		{"missing screen", "application/json", `{"event":"screen_viewed"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			endpoint, captured := captureCollector(t)
			opts := Options{ScreenClaimsPath: filepath.Join(t.TempDir(), "screens")}
			reporter := captureReporter(t, endpoint, opts)
			srv := server.New(config.Config{Host: "127.0.0.1", Port: 8080}, dbtest.OpenTestDB(t), nil, server.WithTelemetryCapture(reporter.CaptureHandler()))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:8080/api/v1/telemetry/events", strings.NewReader(tc.body))
			req.Header.Set("Origin", "http://127.0.0.1:8080")
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			require.NoError(t, reporter.Close())
			assert.Empty(t, captured())
			_, err := os.Stat(opts.ScreenClaimsPath)
			require.ErrorIs(t, err, os.ErrNotExist)
			retry := captureReporter(t, endpoint, opts)
			postCapture(t, retry.CaptureHandler(), valid, http.StatusAccepted)
			require.NoError(t, retry.Close())
			assert.Len(t, captured(), 1)
		})
	}
}

func TestScreenViewDisabled(t *testing.T) {
	t.Setenv(EnabledEnv, "0")
	t.Setenv(GenericEnabledEnv, "1")
	path := filepath.Join(t.TempDir(), "screens")
	reporter, err := NewReporter(Options{ScreenClaimsPath: path})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	postCapture(t, reporter.CaptureHandler(), `{"event":"screen_viewed","properties":{"screen":"sessions"}}`, http.StatusAccepted)
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestScreenViewClaimsAcrossDaemonRestarts(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	endpoint, captured := captureCollector(t)
	cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-id"}
	legacy := "install-id " + time.Now().UTC().Format(time.DateOnly) + " sessions"
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "telemetry-screen-views"), []byte(legacy), 0o600))
	for range 2 {
		require.NoError(t, cfg.MigrateTelemetryScreenClaims())
		reporter := captureReporter(t, endpoint, Options{ScreenClaimsPath: cfg.TelemetryScreenClaimsPath()})
		for _, screen := range []string{"sessions", "usage", "sessions"} {
			postCapture(t, reporter.CaptureHandler(), `{"event":"screen_viewed","properties":{"screen":"`+screen+`","surface":"web"}}`, http.StatusAccepted)
		}
		require.NoError(t, reporter.Close())
	}
	moved, err := filepath.Glob(cfg.TelemetryScreenClaimsPath() + ".unreadable-*")
	require.NoError(t, err)
	require.Len(t, moved, 1)
	data, err := os.ReadFile(moved[0])
	require.NoError(t, err)
	assert.Equal(t, legacy, string(data))
	sent := captured()
	require.Len(t, sent, 1)
	assert.Equal(t, "usage", sent[0]["screen"])
	assert.Equal(t, "web", sent[0]["surface"])
}

func captureCollector(t *testing.T) (string, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var sent []map[string]any
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var payload struct {
			Batch []struct {
				Properties map[string]any `json:"properties"`
			} `json:"batch"`
		}
		if !assert.NoError(t, json.NewDecoder(req.Body).Decode(&payload)) {
			http.Error(w, "invalid capture batch", http.StatusBadRequest)
			return
		}
		mu.Lock()
		for _, item := range payload.Batch {
			sent = append(sent, item.Properties)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	return collector.URL, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), sent...)
	}
}

func captureReporter(t *testing.T, endpoint string, opts Options) *Reporter {
	t.Helper()
	if opts.ScreenClaimsPath == "" {
		opts.ScreenClaimsPath = filepath.Join(t.TempDir(), "screens")
	}
	client, err := kittelemetry.NewReporter(kittelemetry.Options{
		APIKey: "phc_test", Application: application, EnvPrefix: envPrefix,
		DistinctID: "install-id", Source: "daemon", Endpoint: endpoint,
	}, allowedEventOptions(opts)...)
	require.NoError(t, err)
	return &Reporter{client: client}
}

func postCapture(t *testing.T, handler http.Handler, body string, status int) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://127.0.0.1:8080/api/v1/telemetry/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, status, rec.Code, rec.Body.String())
}
