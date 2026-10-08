package telemetry

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	reporter := captureReporter(t, endpoint, Options{
		AgentTypes: []string{"freebuff"}, InsightKinds: []string{"daily_activity"},
		ClaimScreenView: func(_ string, send func() error) (string, bool, error) {
			return time.Now().UTC().Format(time.DateOnly), true, send()
		},
	})
	srv := server.New(config.Config{Host: "127.0.0.1", Port: 8080},
		dbtest.OpenTestDB(t), nil, server.WithTelemetryCapture(reporter.CaptureHandler()))
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
	}
	for _, c := range cases {
		properties := map[string]any{c.key: c.value, "query": "secret prompt"}
		if c.event == EventScreenViewed {
			// Each row checks filtering independently of daily deduplication.
			reporter.screenViews = make(map[string]string)
			if c.key == "surface" {
				properties["screen"] = "sessions"
			}
		}
		body, err := json.Marshal(map[string]any{"event": c.event, "properties": properties})
		require.NoError(t, err)
		postCapture(t, srv.Handler(), string(body), http.StatusAccepted)
	}
	reporter.screenViews = make(map[string]string)
	post := func(body string, status int) { postCapture(t, srv.Handler(), body, status) }
	body := `{"event":"screen_viewed","properties":{"screen":"sessions","surface":"web"}}`
	post(`{"event":"screen_viewed"}`, 202)
	reporter.claimScreenView = func(_ string, _ func() error) (string, bool, error) {
		return "", false, errors.New("claim failed")
	}
	post(body, 500)
	reporter.claimScreenView = func(_ string, send func() error) (string, bool, error) {
		return time.Now().UTC().Format(time.DateOnly), true, send()
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"event":`, 400},
		{`{"e_vent":"app_opened"}`, 400},
	} {
		post(tc.body, tc.status)
	}

	diskClaims := 0
	reporter.claimScreenView = func(screen string, _ func() error) (string, bool, error) {
		assert.Equal(t, "recall", screen)
		diskClaims++
		return time.Now().UTC().Format(time.DateOnly), false, nil
	}
	post(`{"event":"screen_viewed","properties":{"screen":"recall"}}`, 202)
	post(`{"event":"screen_viewed","properties":{"screen":"recall"}}`, 202)
	assert.Equal(t, 1, diskClaims)
	reporter.claimScreenView = func(_ string, send func() error) (string, bool, error) {
		return time.Now().UTC().Format(time.DateOnly), true, send()
	}
	post(`{"event":" screen_viewed ","properties":{"screen":"sessions","surface":"web"}}`, 202)
	for _, contentType := range []string{"", "text/plain"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/telemetry/events",
			strings.NewReader(`{"event":"`+EventScreenViewed+`","properties":{"screen":"usage"}} {}`))
		req.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		reporter.CaptureHandler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusAccepted, rec.Code, "%s %s", contentType, EventScreenViewed)
	}
	reporter.screenViews["sessions"] = time.Now().UTC().Add(-24 * time.Hour).Format(time.DateOnly)
	post(body, 202)
	require.NoError(t, reporter.Close())

	sent := captured()
	require.Len(t, sent, len(cases)-1+3)
	i := 0
	for _, c := range cases {
		if c.event == EventScreenViewed && c.key == "screen" && !c.kept {
			continue
		}
		assert.NotContains(t, sent[i], "query", c.event)
		value, ok := sent[i][c.key]
		if c.kept {
			assert.Equal(t, c.value, value, c.event)
		} else {
			assert.False(t, ok, "%s %s=%v should be dropped", c.event, c.key, value)
		}
		i++
	}
	flow := sent[len(cases)-1:]
	assert.Equal(t, "web", flow[0]["surface"])
	var screens []string
	for _, item := range flow {
		if screen, ok := item["screen"].(string); ok {
			screens = append(screens, screen)
		}
	}
	assert.Equal(t, []string{"sessions", "usage", "sessions"}, screens)
}

func TestScreenViewDisabled(t *testing.T) {
	t.Setenv(EnabledEnv, "0")
	t.Setenv(GenericEnabledEnv, "1")
	reporter, err := NewReporter(Options{
		ClaimScreenView: func(string, func() error) (string, bool, error) {
			assert.Fail(t, "disabled reporter claimed a screen view")
			return "", false, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://127.0.0.1:8080/api/v1/telemetry/events", strings.NewReader(`{"event":"screen_viewed","properties":{"screen":"sessions"}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	reporter.CaptureHandler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.JSONEq(t, `{"status":"disabled"}`, rec.Body.String())
}

func TestScreenViewRemembersClaimedDay(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	endpoint, captured := captureCollector(t)
	claims := 0
	reporter := captureReporter(t, endpoint, Options{
		ClaimScreenView: func(screen string, send func() error) (string, bool, error) {
			assert.Equal(t, "sessions", screen)
			claims++
			require.NoError(t, send())
			day := time.Now().UTC()
			if claims == 1 {
				day = day.Add(-24 * time.Hour)
			}
			return day.Format(time.DateOnly), true, errors.New("saving claim failed")
		},
	})
	for range 3 {
		postCapture(t, reporter.CaptureHandler(), `{"event":"screen_viewed","properties":{"screen":"sessions"}}`, http.StatusAccepted)
	}
	require.NoError(t, reporter.Close())
	assert.Equal(t, 2, claims)
	assert.Len(t, captured(), 2)
}

func TestScreenViewClaimsAcrossDaemonRestarts(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	endpoint, captured := captureCollector(t)
	cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-id"}
	for range 2 {
		reporter := captureReporter(t, endpoint, Options{ClaimScreenView: cfg.ClaimScreenView})
		handler := reporter.CaptureHandler()
		for _, screen := range []string{"sessions", "usage", "sessions", "unknown"} {
			postCapture(t, handler, `{"event":"screen_viewed","properties":{"screen":"`+screen+`","surface":"web"}}`, http.StatusAccepted)
		}
		require.NoError(t, reporter.Close())
	}
	sent := captured()
	require.Len(t, sent, 2)
	assert.ElementsMatch(t, []any{"sessions", "usage"}, []any{sent[0]["screen"], sent[1]["screen"]})
	for _, item := range sent {
		assert.Equal(t, "web", item["surface"])
	}
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
	client, err := kittelemetry.NewReporter(kittelemetry.Options{
		APIKey: "phc_test", Application: application, EnvPrefix: envPrefix,
		DistinctID: "install-id", Source: "daemon", Endpoint: endpoint,
	}, allowedEventOptions(opts)...)
	require.NoError(t, err)
	return &Reporter{client: client, claimScreenView: opts.ClaimScreenView}
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
