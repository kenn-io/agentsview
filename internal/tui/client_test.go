package tui

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
