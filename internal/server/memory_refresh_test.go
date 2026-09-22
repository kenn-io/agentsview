package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestMemoryRefreshRouteQueuesBackgroundWork(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	requests := 0
	srv := New(config.Config{
		Host: "127.0.0.1", Port: 8080,
		RequireAuth: true, AuthToken: "test-token",
	}, database, nil,
		WithMemoryRefreshRequester(func() { requests++ }))

	unauthorized := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://127.0.0.1:8080/api/v1/memory/refresh", nil)
	unauthorized.Header.Set("Origin", "http://127.0.0.1:8080")
	unauthorizedRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unauthorizedRec, unauthorized)
	assert.Equal(t, http.StatusUnauthorized, unauthorizedRec.Code)
	assert.Zero(t, requests)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://127.0.0.1:8080/api/v1/memory/refresh", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"queued":true}`, rec.Body.String())
	assert.Equal(t, 1, requests)
}

func TestMemoryRefreshRouteUnavailableWithoutWritableScheduler(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	srv := New(config.Config{Host: "127.0.0.1", Port: 8080}, database, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://127.0.0.1:8080/api/v1/memory/refresh", nil)
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "automatic refresh is unavailable")
}
