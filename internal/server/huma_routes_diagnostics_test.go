package server_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiagnosticAppendRoute(t *testing.T) {
	te := setupWithServerOpts(t, nil)
	id := uuid.NewV7().String()
	body := fmt.Sprintf(`{"id":%q,"source":"worker","message":"launch failed","occurred_at":"2026-01-01T23:55:00Z"}`, id)
	post := func(body, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/diagnostics", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remote
		w := httptest.NewRecorder()
		te.handler.ServeHTTP(w, req)
		return w
	}
	assertStatus(t, post(body, "203.0.113.5:1234"), http.StatusForbidden)
	assertStatus(t, post(body, "127.0.0.1:1234"), http.StatusOK)
	assertStatus(t, post(body, "127.0.0.1:1234"), http.StatusOK)
	assertStatus(t, post(strings.Replace(body, id, strings.ToUpper(id), 1), "127.0.0.1:1234"), http.StatusOK)
	withoutTime := fmt.Sprintf(`{"id":%q,"source":"worker","message":"launch failed"}`, id)
	assertStatus(t, post(withoutTime, "127.0.0.1:1234"), http.StatusOK)
	conflict := strings.Replace(body, "launch failed", "other failure", 1)
	assertStatus(t, post(conflict, "127.0.0.1:1234"), http.StatusConflict)
	future := fmt.Sprintf(`{"id":%q,"source":"worker","message":"failure","occurred_at":%q}`, uuid.NewV7().String(), time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339))
	assertStatus(t, post(future, "127.0.0.1:1234"), http.StatusUnprocessableEntity)
	invalid := fmt.Sprintf(`{"id":%q,"source":"worker","message":"fail\u0000ure"}`, uuid.NewV7().String())
	assertStatus(t, post(invalid, "127.0.0.1:1234"), http.StatusUnprocessableEntity)
	invalidTime := fmt.Sprintf(`{"id":%q,"source":"worker","message":"failure","occurred_at":"0000-01-01T00:00:00+01:00"}`, uuid.NewV7().String())
	assertStatus(t, post(invalidTime, "127.0.0.1:1234"), http.StatusUnprocessableEntity)
	zeroTime := fmt.Sprintf(`{"id":%q,"source":"worker","message":"failure","occurred_at":"0001-01-01T00:00:00Z"}`, uuid.NewV7().String())
	assertStatus(t, post(zeroTime, "127.0.0.1:1234"), http.StatusUnprocessableEntity)
	events, err := te.db.DiagnosticSignalsForDate(t.Context(), "2026-01-01", time.UTC)
	require.NoError(t, err)
	assert.Len(t, events, 1)
}
