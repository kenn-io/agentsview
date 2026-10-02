package telemetry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kittelemetry "go.kenn.io/kit/telemetry"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/insight"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
)

// absent marks a case whose property the daemon must drop while still sending the event.
const absent = "<absent>"

type captureCase struct {
	event string
	key   string
	props map[string]any
	want  string
}

func keptCase(event, key, value string) captureCase {
	return captureCase{event: event, key: key, props: map[string]any{key: value}, want: value}
}

func droppedCase(event, key string, props map[string]any) captureCase {
	return captureCase{event: event, key: key, props: props, want: absent}
}

type batchMessage struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
}

func TestCoreActionCaptureEndpoint(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")

	var mu sync.Mutex
	var bodies [][]byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/batch") {
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)

	client, err := kittelemetry.NewPostHogReporter(kittelemetry.PostHogOptions{
		APIKey:      "phc_test",
		Application: application,
		EnvPrefix:   envPrefix,
		DistinctID:  "anonymous-install-id",
		Version:     "v1.2.3",
		Commit:      "abc123",
		Source:      "daemon",
		Endpoint:    collector.URL,
	}, allowedEventOptions()...)
	require.NoError(t, err)
	reporter := &Reporter{client: client}
	require.True(t, reporter.Enabled())

	srv := server.New(config.Config{Host: "127.0.0.1", Port: 8080},
		dbtest.OpenTestDB(t), nil, server.WithTelemetryCapture(reporter.CaptureHandler()))

	var cases []captureCase
	for _, v := range []string{"text", "semantic", "hybrid"} {
		cases = append(cases, keptCase(EventSearchRun, "query_type", v))
	}
	cases = append(cases, captureCase{
		event: EventSearchRun, key: "query_type",
		props: map[string]any{"query_type": "text", "query": "secret prompt"},
		want:  "text",
	})
	for _, v := range []string{"html", "csv"} {
		cases = append(cases, keptCase(EventExportRun, "format", v))
	}
	for _, v := range []string{"sessions", "usage", "activity", "trends", "quality"} {
		cases = append(cases, keptCase(EventAnalyticsViewed, "page", v))
	}
	kinds := []string{"daily_activity", "agent_analysis"}
	for k := range insight.ValidCannedKinds {
		kinds = append(kinds, string(k))
	}
	for _, v := range kinds {
		cases = append(cases, keptCase(EventInsightGenerated, "kind", v))
	}
	agents := []string{"freebuff", "codex", "hermes"}
	for _, def := range parser.Registry {
		agents = append(agents, string(def.Type))
	}
	for _, v := range agents {
		cases = append(cases, keptCase(EventSessionViewed, "agent", v))
	}
	cases = append(cases,
		droppedCase(EventSearchRun, "query_type", map[string]any{"query_type": "regex"}),
		droppedCase(EventSessionViewed, "agent", map[string]any{"agent": "/Users/alice/secret"}),
		droppedCase(EventExportRun, "format", map[string]any{"format": "pdf"}),
		droppedCase(EventExportRun, "format", map[string]any{"format": "markdown"}),
		droppedCase(EventInsightGenerated, "kind", map[string]any{"kind": "llm_canned"}),
		droppedCase(EventAnalyticsViewed, "page", map[string]any{"page": "settings"}),
		droppedCase(EventAnalyticsViewed, "page", map[string]any{"page": 3}),
		droppedCase(EventAnalyticsViewed, "page", map[string]any{"page": map[string]any{"x": "y"}}),
		droppedCase(EventAnalyticsViewed, "page", map[string]any{"application": "other"}),
	)

	for _, c := range cases {
		body, err := json.Marshal(map[string]any{"event": c.event, "properties": c.props})
		require.NoError(t, err)
		rec := postCapture(t, srv, string(body))
		require.Equal(t, http.StatusAccepted, rec.Code, "%s: %s", body, rec.Body.String())
		assert.JSONEq(t, `{"status":"queued"}`, rec.Body.String(), string(body))
	}
	rec := postCapture(t, srv, `{"event":"unknown_event"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	require.NoError(t, client.Close())

	mu.Lock()
	defer mu.Unlock()
	got := map[string][]string{}
	for _, body := range bodies {
		var payload struct {
			Batch []batchMessage `json:"batch"`
		}
		require.NoError(t, json.Unmarshal(body, &payload))
		for _, msg := range payload.Batch {
			assert.Equal(t, "anonymous-install-id", msg.DistinctID)
			assert.Equal(t, "agentsview", msg.Properties["application"], msg.Event)
			assert.NotContains(t, msg.Properties, "query", msg.Event)
			key := propertyKey(msg.Event)
			value, ok := msg.Properties[key]
			if !ok {
				got[msg.Event] = append(got[msg.Event], absent)
				continue
			}
			s, isString := value.(string)
			require.True(t, isString, "%s %s = %v", msg.Event, key, value)
			got[msg.Event] = append(got[msg.Event], s)
		}
	}

	want := map[string][]string{}
	for _, c := range cases {
		want[c.event] = append(want[c.event], c.want)
	}
	assert.NotContains(t, got, "unknown_event")
	require.Len(t, got, len(want))
	for event, values := range want {
		assert.ElementsMatch(t, values, got[event], event)
	}
}

func propertyKey(event string) string {
	switch event {
	case EventSearchRun:
		return "query_type"
	case EventSessionViewed:
		return "agent"
	case EventExportRun:
		return "format"
	case EventInsightGenerated:
		return "kind"
	case EventAnalyticsViewed:
		return "page"
	}
	return ""
}

func postCapture(t *testing.T, srv *server.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://127.0.0.1:8080/api/v1/telemetry/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}
