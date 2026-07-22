package server_test

import (
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
	req := httptest.NewRequest(method, path, strings.NewReader(body))
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

	w := te.put(t, "/api/v1/sessions/s1/star", `{}`)
	require.Equal(t, http.StatusServiceUnavailable, w.Code,
		"body: %s", w.Body.String())
	assert.Equal(t, "5", w.Header().Get("Retry-After"),
		"a writer-closed write must advertise Retry-After")
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
