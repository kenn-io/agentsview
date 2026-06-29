package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

func TestRemoteSyncAuthRequiredWhenGlobalAuthDisabled(t *testing.T) {
	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         8080,
		AuthToken:    "remote-token",
		RequireAuth:  false,
		WriteTimeout: 30 * time.Second,
	}, nil, nil)

	called := false
	handler := srv.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		assert.True(t, isRemoteAuth(r))
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote-sync/targets", nil)
	req.Header.Set("Authorization", "Bearer remote-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.True(t, called)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestRemoteSyncAuthRejectsMissingTokenWhenGlobalAuthDisabled(t *testing.T) {
	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         8080,
		AuthToken:    "remote-token",
		RequireAuth:  false,
		WriteTimeout: 30 * time.Second,
	}, nil, nil)

	handler := srv.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.FailNow(t, "handler should not run")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote-sync/targets", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
