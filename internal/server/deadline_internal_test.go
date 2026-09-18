package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestHandlers_Internal_DeadlineExceeded(t *testing.T) {
	t.Parallel()
	s := testServer(t, 30*time.Second)

	// Seed a session just in case handlers check for existence before context.
	started := "2025-01-15T10:00:00Z"
	sess := db.Session{
		ID:        "s1",
		Project:   "test-proj",
		StartedAt: &started,
	}
	require.NoError(t, s.db.UpsertSession(t.Context(), sess))

	tests := []struct {
		name        string
		path        string
		requiresFTS bool
	}{
		{"ListSessions", "/api/v1/sessions", false},
		{"GetSession", "/api/v1/sessions/s1", false},
		{"GetMessages", "/api/v1/sessions/s1/messages", false},
		{"GetStats", "/api/v1/stats", false},
		{"ListProjects", "/api/v1/projects", false},
		{"ListMachines", "/api/v1/machines", false},
		{"Search", "/api/v1/search?q=test", true},
		{"GetSessionActivity", "/api/v1/sessions/s1/activity", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.requiresFTS && !s.db.HasFTS(t.Context()) {
				t.Skip("skipping test: no FTS support")
			}
			ctx, cancel := expiredCtx(t)
			defer cancel()

			req := httptest.NewRequestWithContext(ctx, http.MethodGet, tt.path, nil)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()

			s.mux.ServeHTTP(w, req)

			assertRecorderStatus(t, w, http.StatusGatewayTimeout)
			assertContentType(t, w, "application/json")
		})
	}
}
