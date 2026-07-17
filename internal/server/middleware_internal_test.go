package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A non-positive write timeout must disable the deadline instead of wrapping the
// handler in http.TimeoutHandler with a 0 duration, which would fire immediately
// and 503 every request.
func TestWithTimeout_NonPositiveDisablesDeadline(t *testing.T) {
	t.Parallel()
	s := testServer(t, 0)

	called := false
	h := s.withTimeout(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/recall/entries", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.True(t, called, "handler should run when the write timeout is disabled")
	assert.Equal(t, http.StatusOK, w.Code)
}
