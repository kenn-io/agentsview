package server_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type starredHandlerResponse struct {
	SessionIDs []string `json:"session_ids"`
}

func (te *testEnv) put(
	t *testing.T, path string, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	return te.requestJSON(t, http.MethodPut, path, body)
}

func (te *testEnv) patch(
	t *testing.T, path string, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	return te.requestJSON(t, http.MethodPatch, path, body)
}

func (te *testEnv) requestJSON(
	t *testing.T, method, path string, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	return w
}

func TestStarSessionReturns503WhileWriterClosed(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)

	// Close the writer to simulate the maintenance-pass barrier window while the
	// daemon's worker rebuilds the archive; readers keep serving.
	require.NoError(t, te.db.CloseWriter())
	defer func() { assert.NoError(t, te.db.ReopenWriter()) }()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/sessions/s1/star"},
		{http.MethodPost, "/api/v1/export/conversations/initialize"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := te.requestJSON(t, tc.method, tc.path, `{}`)
			require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
			assert.Equal(t, "5", w.Header().Get("Retry-After"))
			assert.Contains(t, w.Body.String(), "archive is read-only for a maintenance pass; retry after it finishes")
		})
	}
}

func TestScopedUpgradeFreshnessOutlivesWriterPause(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)
	require.NoError(t, te.db.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			"INSERT INTO stats(key, value) VALUES ('scoped_upgrade_pending', 1)",
		)
		return err
	}))

	type statusResponse struct {
		ArchiveStale   bool `json:"archive_stale"`
		UpgradePending bool `json:"upgrade_pending"`
		WritesPaused   bool `json:"writes_paused"`
	}
	assertStatus := func(w *httptest.ResponseRecorder, upgradePending, writesPaused bool) {
		t.Helper()
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		status := decode[statusResponse](t, w)
		assert.False(t, status.ArchiveStale)
		assert.Equal(t, upgradePending, status.UpgradePending)
		assert.Equal(t, writesPaused, status.WritesPaused)
	}

	assertStatus(te.get(t, "/api/v1/sync/status"), true, false)
	read := te.get(t, "/api/v1/sessions")
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	assert.Equal(t, "true", read.Header().Get("X-Agentsview-Archive-Stale"))

	require.NoError(t, te.db.CloseWriter())
	t.Cleanup(func() {
		if te.db.WriterClosed() {
			assert.NoError(t, te.db.ReopenWriter())
		}
	})
	assertStatus(te.get(t, "/api/v1/sync/status"), true, true)
	blocked := te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusServiceUnavailable, blocked.Code, blocked.Body.String())
	assert.Equal(t, "5", blocked.Header().Get("Retry-After"))

	require.NoError(t, te.db.ReopenWriter())
	assertStatus(te.get(t, "/api/v1/sync/status"), true, false)
	assert.Equal(t, "true", te.get(t, "/api/v1/sessions").Header().Get("X-Agentsview-Archive-Stale"))

	require.NoError(t, te.db.CompleteScopedUpgrade(t.Context()))
	assertStatus(te.get(t, "/api/v1/sync/status"), false, false)
	read = te.get(t, "/api/v1/sessions")
	assert.Empty(t, read.Header().Get("X-Agentsview-Archive-Stale"))
	assert.Equal(t, http.StatusNoContent, te.put(t, "/api/v1/sessions/s1/star", `{}`).Code)
}

func TestStarredHandlers(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)
	te.seedSession(t, "s2", "beta", 2)

	w := te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.get(t, "/api/v1/starred")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list := decode[starredHandlerResponse](t, w)
	assert.Equal(t, []string{"s1"}, list.SessionIDs)

	w = te.del(t, "/api/v1/sessions/s1/star")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.get(t, "/api/v1/starred")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list = decode[starredHandlerResponse](t, w)
	assert.Empty(t, list.SessionIDs)

	w = te.put(t, "/api/v1/sessions/missing/star", `{}`)
	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	assertErrorResponse(t, w, "session not found")

	w = te.post(t, "/api/v1/starred/bulk", `{"session_ids":["s1","s2","missing"]}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.get(t, "/api/v1/starred")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	list = decode[starredHandlerResponse](t, w)
	assert.ElementsMatch(t, []string{"s1", "s2"}, list.SessionIDs)
}
