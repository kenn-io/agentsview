//go:build pgtest

package postgres_test

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/sessionwatch"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHostedServiceAndHTTPPublicBoundary(t *testing.T) {
	store, split, pin := postgres.HostedPublicFixture(t)
	backend := service.NewReadOnlyBackend(store)
	session, err := backend.Get(t.Context(), "codex:portable")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "codex:portable", session.ID)
	cfg, err := config.Default()
	require.NoError(t, err)
	cfg.DataDir = t.TempDir()
	app := server.New(cfg, store, nil)
	request := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "localhost:8080"
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{"/api/v1/sessions/codex:portable", "/api/v1/sessions/codex:portable/messages", "/api/v1/sessions/codex:portable/export"} {
		w := request(path)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		if strings.HasSuffix(path, "/export") {
			assert.Contains(t, w.Body.String(), "readable transcript")
		} else {
			assert.Contains(t, w.Body.String(), "codex:portable")
		}
		assert.NotContains(t, w.Body.String(), "raw-row-")
	}
	pin()
	w := request("/api/v1/pins")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "remember this")
	assert.Contains(t, w.Body.String(), "message_key")
	split()
	w = request("/api/v1/sessions/codex:portable")
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	var conflict struct {
		State    string   `json:"state"`
		Variants []string `json:"variants"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &conflict))
	assert.Equal(t, "ambiguous", conflict.State)
	require.Len(t, conflict.Variants, 2)
	for _, v := range conflict.Variants {
		assert.True(t, strings.HasPrefix(v, "codex:portable~"))
	}
	w = request("/api/v1/sessions/codex:portable/watch")
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
}

func TestHostedWatchReportsConflictAndStops(t *testing.T) {
	store, split, _ := postgres.HostedPublicFixture(t)
	restore := sessionwatch.SetTimingsForTest(10*time.Millisecond, time.Second)
	defer restore()
	backend := service.NewReadOnlyBackend(store)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	events, err := backend.Watch(ctx, "codex:portable")
	require.NoError(t, err)
	split()
	select {
	case event, ok := <-events:
		require.True(t, ok)
		assert.Equal(t, "session.identity", event.Event)
		var state db.SessionWatchState
		require.NoError(t, json.Unmarshal([]byte(event.Data), &state))
		assert.Equal(t, "ambiguous", state.State)
		require.Len(t, state.Variants, 2)
	case <-ctx.Done():
		t.Fatal("watch did not report conflict")
	}
	select {
	case _, ok := <-events:
		assert.False(t, ok)
	case <-ctx.Done():
		t.Fatal("watch did not close")
	}
}
