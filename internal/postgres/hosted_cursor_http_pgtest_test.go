//go:build pgtest

package postgres_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
)

func TestHostedCursorHTTPBadRequest(t *testing.T) {
	store, split, _ := postgres.HostedPublicFixture(t)
	cfg, err := config.Default()
	require.NoError(t, err)
	cfg.DataDir = t.TempDir()
	app := server.New(cfg, store, nil)
	cursor := store.EncodeCursor(db.SessionCursor{ID: "codex:portable", EndedAt: "2026-01-01T00:00:00Z"})
	require.NotEmpty(t, cursor)
	_, err = store.DecodeCursor(cursor)
	require.NoError(t, err)
	split()

	for _, path := range []string{"/api/v1/sessions", "/api/v1/sessions/sidebar-index"} {
		for name, token := range map[string]string{"malformed": "invalid", "stale": cursor} {
			t.Run(path+"/"+name, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, path+"?cursor="+url.QueryEscape(token), nil)
				request.Host = "localhost:8080"
				response := httptest.NewRecorder()
				app.Handler().ServeHTTP(response, request)
				assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
				assert.Contains(t, response.Body.String(), "invalid cursor")
			})
		}
	}
}
