package tui

import (
	"context"
	"io"
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
	"go.kenn.io/agentsview/internal/money"
)

func TestClientMutationAuthenticatesAndSetsOrigin(t *testing.T) {
	var gotMethod, gotPath, gotOrigin, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.EscapedPath()
		gotOrigin, gotAuth = r.Header.Get("Origin"), r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	client := NewClient(server.URL, "secret-token", false)
	message, err := client.Mutate(context.Background(), Mutation{
		Kind: "rename", SessionID: "session/one", Value: "New name",
	})

	require.NoError(t, err)
	assert.Equal(t, "renamed session", message)
	assert.Equal(t, http.MethodPatch, gotMethod)
	assert.Equal(t, "/api/v1/sessions/session%2Fone/rename", gotPath)
	assert.Equal(t, server.URL, gotOrigin)
	assert.Equal(t, "Bearer secret-token", gotAuth)
}

func TestClientStartupSyncRetriesRequiredResync(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/sync":
			w.Header().Set("X-Agentsview-Resync-Required", "true")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"archive data version changed"}`)
		case "/api/v1/resync":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	message, err := NewClient(server.URL, "", false).Mutate(
		context.Background(), Mutation{Kind: "startup-sync"},
	)

	require.NoError(t, err)
	assert.Equal(t, "sync complete", message)
	assert.Equal(t, []string{"/api/v1/sync", "/api/v1/resync"}, paths)
}

func TestClientStartupSyncDoesNotResyncOrdinaryConflict(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"sync already running"}`)
	}))
	t.Cleanup(server.Close)

	_, err := NewClient(server.URL, "", false).Mutate(
		context.Background(), Mutation{Kind: "startup-sync"},
	)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "sync already running", apiErr.Message)
	assert.Equal(t, 1, requests)
}

func TestClientKeepsProvisionedAuthenticationToken(t *testing.T) {
	var request int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request++
		if request == 1 {
			_, _ = io.WriteString(w, `{"require_auth":true,"auth_token":"provisioned"}`)
			return
		}
		assert.Equal(t, "Bearer provisioned", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	client := NewClient(server.URL, "", false)

	_, err := client.Mutate(context.Background(), Mutation{Kind: "require-auth", Flag: true})
	require.NoError(t, err)
	_, err = client.Settings(context.Background())

	require.NoError(t, err)
}

func TestClientImportsClaudeArchiveThroughClaudeAIEndpoint(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "conversations.json")
	require.NoError(t, os.WriteFile(archive, []byte(`[{"name":"test"}]`), 0o600))
	var gotPath, gotName, gotContent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		require.NoError(t, r.ParseMultipartForm(1<<20))
		file, header, err := r.FormFile("file")
		require.NoError(t, err)
		defer file.Close()
		gotName = header.Filename
		body, err := io.ReadAll(file)
		require.NoError(t, err)
		gotContent = string(body)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)

	message, err := NewClient(server.URL, "", false).Mutate(
		context.Background(), Mutation{Kind: "import-claude", Value: archive},
	)

	require.NoError(t, err)
	assert.Equal(t, "imported claude-ai archive", message)
	assert.Equal(t, "/api/v1/import/claude-ai", gotPath)
	assert.Equal(t, "conversations.json", gotName)
	assert.Equal(t, `[{"name":"test"}]`, gotContent)
}

func TestClientExportDoesNotOverwriteExistingFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "new export")
	}))
	t.Cleanup(server.Close)
	target := filepath.Join(t.TempDir(), "session.md")
	require.NoError(t, os.WriteFile(target, []byte("keep me"), 0o600))

	_, err := NewClient(server.URL, "", false).Mutate(context.Background(), Mutation{
		Kind: "export-markdown", SessionID: "s1", Value: target,
	})

	require.Error(t, err)
	content, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, "keep me", string(content))
}

func TestClientLoadsEveryDashboardSurfaceWithSharedFilters(t *testing.T) {
	wantPaths := map[string]bool{
		"/api/v1/analytics/summary": false, "/api/v1/analytics/activity": false,
		"/api/v1/analytics/heatmap": false, "/api/v1/analytics/projects": false,
		"/api/v1/analytics/hour-of-week": false, "/api/v1/analytics/sessions": false,
		"/api/v1/analytics/velocity": false, "/api/v1/analytics/tools": false,
		"/api/v1/analytics/skills": false, "/api/v1/analytics/top-sessions": false,
		"/api/v1/analytics/signals": false,
	}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, known := wantPaths[r.URL.Path]
		assert.True(t, known, "unexpected dashboard path %s", r.URL.Path)
		assert.Equal(t, "app", r.URL.Query().Get("project"))
		wantPaths[r.URL.Path] = true
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)

	data, err := NewClient(server.URL, "", false).LoadPage(
		context.Background(), PageDashboard, PageQuery{Project: "app"},
	)

	require.NoError(t, err)
	assert.NotNil(t, data.Analytics)
	assert.NotNil(t, data.Tools)
	assert.NotNil(t, data.Signals)
	for path, called := range wantPaths {
		assert.True(t, called, "dashboard did not request %s", path)
	}
}

func TestClientLoadsDashboardSurfacesConcurrently(t *testing.T) {
	const surfaceCount = 11
	started := make(chan struct{}, surfaceCount)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	errs := make(chan error, 1)
	go func() {
		_, err := NewClient(server.URL, "", false).LoadPage(
			context.Background(), PageDashboard, PageQuery{},
		)
		errs <- err
	}()

	for range surfaceCount {
		select {
		case <-started:
		case <-time.After(time.Second):
			require.FailNow(t, "dashboard requests did not start concurrently")
		}
	}
	close(release)

	require.NoError(t, <-errs)
}

func TestClientStreamsDashboardSurfacesAsTheyFinish(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/analytics/summary" {
			_, _ = io.WriteString(w, `{"total_sessions":73}`)
			return
		}
		<-release
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	})

	updates, supported := NewClient(server.URL, "", false).LoadPageUpdates(
		context.Background(), PageDashboard, PageQuery{},
	)
	require.True(t, supported)
	select {
	case update := <-updates:
		require.NoError(t, update.Err)
		require.NotNil(t, update.Data.Analytics)
		assert.Equal(t, 73, update.Data.Analytics.TotalSessions)
		assert.False(t, update.Done)
	case <-time.After(time.Second):
		require.FailNow(t, "dashboard summary was held behind slower surfaces")
	}

	releaseOnce.Do(func() { close(release) })
	var final pageUpdate
	for update := range updates {
		final = update
	}
	require.NoError(t, final.Err)
	assert.True(t, final.Done)
	assert.NotNil(t, final.Data.Signals)
}

func TestClientLoadsSessionExtrasConcurrently(t *testing.T) {
	started := make(chan string, 3)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		<-release
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	result := make(chan SessionExtras, 1)
	errs := make(chan error, 1)
	go func() {
		extras, err := NewClient(server.URL, "", false).SessionExtras(context.Background(), "s1")
		result <- extras
		errs <- err
	}()

	seen := make(map[string]bool)
	for range 3 {
		select {
		case path := <-started:
			seen[path] = true
		case <-time.After(time.Second):
			require.FailNow(t, "session extra requests did not start concurrently")
		}
	}
	close(release)
	extras := <-result

	require.NoError(t, <-errs)
	assert.Equal(t, map[string]bool{
		"/api/v1/sessions/s1/activity":       true,
		"/api/v1/sessions/s1/timing-summary": true,
		"/api/v1/sessions/s1/usage":          true,
	}, seen)
	assert.NotNil(t, extras.Activity)
	assert.NotNil(t, extras.Timing)
	assert.NotNil(t, extras.Usage)
}

func TestClientUsesActivityAndInsightPageControls(t *testing.T) {
	var activityQuery, insightQuery map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := make(map[string]string)
		for key := range r.URL.Query() {
			values[key] = r.URL.Query().Get(key)
		}
		switch r.URL.Path {
		case "/api/v1/activity/report":
			activityQuery = values
		case "/api/v1/insights":
			insightQuery = values
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(server.Close)
	client := NewClient(server.URL, "", false)

	_, err := client.LoadPage(context.Background(), PageActivity, PageQuery{
		ActivityPreset: "custom", ActivityDate: "2026-08-01",
		ActivityBucket: "1h", ActivityAutomation: "interactive",
	})
	require.NoError(t, err)
	_, err = client.LoadPage(context.Background(), PageInsights, PageQuery{
		From: "2026-08-01", To: "2026-08-07", InsightType: "weekly",
	})
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"preset": "custom", "date": "2026-08-01", "bucket": "1h",
		"automation": "interactive",
	}, activityQuery)
	assert.Equal(t, map[string]string{
		"date_from": "2026-08-01", "date_to": "2026-08-07", "type": "weekly",
	}, insightQuery)
}

func TestClientUsesExactMoneyForUsageComparison(t *testing.T) {
	var comparisonQuery map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/usage/summary":
			_, _ = io.WriteString(w, `{"from":"2026-08-01","to":"2026-08-02","totals":{"totalCost":{"microdollars":1250001}}}`)
		case "/api/v1/usage/top-sessions":
			_, _ = io.WriteString(w, `[]`)
		case "/api/v1/usage/comparison":
			comparisonQuery = make(map[string]string)
			for key := range r.URL.Query() {
				comparisonQuery[key] = r.URL.Query().Get(key)
			}
			_, _ = io.WriteString(w, `{"priorFrom":"2026-07-30","priorTo":"2026-07-31","priorTotalCost":{"microdollars":500000},"deltaPct":1.5}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	data, err := NewClient(server.URL, "", false).LoadPage(
		context.Background(), PageUsage,
		PageQuery{From: "2026-08-01", To: "2026-08-02"},
	)

	require.NoError(t, err)
	require.NotNil(t, data.UsageComparison)
	assert.Equal(t, "1250001", comparisonQuery["current_microdollars"])
	assert.NotContains(t, comparisonQuery, "current_cost")
	assert.Equal(t, money.Money{Microdollars: 500000},
		data.UsageComparison.PriorTotalCost)
}

func TestClientLoadsUsageSummaryAndTopSessionsConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/usage/summary", "/api/v1/usage/top-sessions":
			started <- r.URL.Path
			<-release
			if r.URL.Path == "/api/v1/usage/top-sessions" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		case "/api/v1/usage/comparison":
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	errs := make(chan error, 1)
	go func() {
		_, err := NewClient(server.URL, "", false).LoadPage(
			context.Background(), PageUsage, PageQuery{},
		)
		errs <- err
	}()

	seen := make(map[string]bool)
	for range 2 {
		select {
		case path := <-started:
			seen[path] = true
		case <-time.After(time.Second):
			require.FailNow(t, "usage requests did not start concurrently")
		}
	}
	close(release)

	require.NoError(t, <-errs)
	assert.Equal(t, map[string]bool{
		"/api/v1/usage/summary":      true,
		"/api/v1/usage/top-sessions": true,
	}, seen)
}

func TestClientStreamsUsageRankingBeforeSlowerSummary(t *testing.T) {
	releaseSummary := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/usage/summary":
			<-releaseSummary
			_, _ = io.WriteString(w, `{}`)
		case "/api/v1/usage/top-sessions":
			_, _ = io.WriteString(w, `[{"sessionId":"s1","displayName":"fast result","agent":"codex","cost":{"microdollars":1000},"totalTokens":7}]`)
		case "/api/v1/usage/comparison":
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseSummary) })
		server.Close()
	})

	updates, supported := NewClient(server.URL, "", false).LoadPageUpdates(
		context.Background(), PageUsage, PageQuery{},
	)
	require.True(t, supported)
	select {
	case update := <-updates:
		require.NoError(t, update.Err)
		require.Len(t, update.Data.UsageTopSessions, 1)
		assert.Equal(t, "fast result", update.Data.UsageTopSessions[0].DisplayName)
		assert.Nil(t, update.Data.Usage)
		assert.False(t, update.Done)
	case <-time.After(time.Second):
		require.FailNow(t, "usage ranking was held behind the slower summary")
	}

	releaseOnce.Do(func() { close(releaseSummary) })
	var final pageUpdate
	for update := range updates {
		final = update
	}
	require.NoError(t, final.Err)
	assert.True(t, final.Done)
	assert.NotNil(t, final.Data.Usage)
	assert.NotNil(t, final.Data.UsageComparison)
	assert.Len(t, final.Data.UsageTopSessions, 1)
}

func TestClientReturnsDaemonErrorMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"build already running"}`)
	}))
	t.Cleanup(server.Close)

	_, err := NewClient(server.URL, "", false).Mutate(
		context.Background(), Mutation{Kind: "embeddings-build"},
	)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.Status)
	assert.Equal(t, "build already running", apiErr.Message)
}

func TestClientReturnsStreamingMutationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: log\ndata: {}\n\nevent: error\ndata: {\"message\":\"agent failed\"}\n\n")
	}))
	t.Cleanup(server.Close)

	_, err := NewClient(server.URL, "", false).Mutate(context.Background(), Mutation{
		Kind: "generate-insight", Value: "daily_activity|2026-08-01|2026-08-01|app",
	})

	require.EqualError(t, err, "agent failed")
}

func TestClientReturnsRemoteSyncFailureFromDoneEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: done\ndata: {\"failures\":[{\"error\":\"remote unavailable\"}]}\n\n")
	}))
	t.Cleanup(server.Close)

	_, err := NewClient(server.URL, "", false).Mutate(context.Background(), Mutation{
		Kind: "sync-remote", Value: "host",
	})

	require.EqualError(t, err, "remote unavailable")
}

func TestScanSSEEmitsTypedEvents(t *testing.T) {
	out := make(chan ServerEvent, 2)
	input := "event: session-updated\ndata: {\"id\":\"s1\"}\n\nevent: sync-complete\ndata: {}\n\n"

	scanSSE(context.Background(), strings.NewReader(input), out)
	close(out)

	var events []ServerEvent
	for event := range out {
		events = append(events, event)
	}
	require.Len(t, events, 2)
	assert.Equal(t, ServerEvent{Event: "session-updated", Data: `{"id":"s1"}`}, events[0])
	assert.Equal(t, ServerEvent{Event: "sync-complete", Data: `{}`}, events[1])
}
