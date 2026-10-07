package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestSetSessionLabelsMapsRemoteReadOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/api/v1/sessions/worker/labels", r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(srv.Close)
	backend := NewHTTPBackend(srv.URL, "", false, "").(*httpBackend)
	backend.apiVersion = service.SessionAnnotationsAPIVersion
	_, err := backend.SetSessionLabels(t.Context(), "worker", []string{"nightly"})
	require.ErrorIs(t, err, db.ErrReadOnly)
	assert.ErrorContains(t, err, "stop the read-only serve process and use the local DB, or start a local daemon")
}
