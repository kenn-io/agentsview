package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddleware_Timeout(t *testing.T) {
	t.Parallel()
	te := setup(t)
	// Seed some data so handlers don't fail with 404 before checking context
	te.seedSession(t, "s1", "my-app", 10)
	te.seedMessages(t, "s1", 10)

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"ListSessions", http.MethodGet, "/api/v1/sessions"},
		{"GetSession", http.MethodGet, "/api/v1/sessions/s1"},
		{"GetMessages", http.MethodGet, "/api/v1/sessions/s1/messages"},
		{"GetStats", http.MethodGet, "/api/v1/stats"},
		{"ListProjects", http.MethodGet, "/api/v1/projects"},
		{"ListMachines", http.MethodGet, "/api/v1/machines"},
		{"GetSessionActivity", http.MethodGet, "/api/v1/sessions/s1/activity"},
		{"GetSessionUsage", http.MethodGet, "/api/v1/sessions/s1/usage"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := expiredContext(t)
			defer cancel()

			req := httptest.NewRequestWithContext(ctx, tt.method, tt.path, nil).WithContext(ctx)
			w := httptest.NewRecorder()
			te.handler.ServeHTTP(w, req)

			assertTimeoutRace(t, w)
		})
	}
}
