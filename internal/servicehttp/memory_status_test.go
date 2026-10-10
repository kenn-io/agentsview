package servicehttp_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestMemoryStatusResponses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		httpStatus int
		want       service.MemoryReadinessState
		wantReason string
	}{
		{"older server", http.StatusNotFound, service.MemoryUnknown, "unsupported"},
		{"ready", http.StatusOK, service.MemoryReady, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/memory/status", r.URL.Path)
				if tc.httpStatus == http.StatusNotFound {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, service.MemoryStatus{Status: service.MemoryReady}))
			}))
			t.Cleanup(server.Close)
			backend := servicehttp.NewHTTPBackend(server.URL, "", true, "")
			status, err := service.GetMemoryStatus(t.Context(), backend)
			require.NoError(t, err)
			assert.Equal(t, tc.want, status.Status)
			assert.Equal(t, tc.wantReason, status.Lexical.Reason)
		})
	}
}

func TestMemoryStatusPreservesHTTPFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	backend := servicehttp.NewHTTPBackend(server.URL, "", true, "")

	_, err := service.GetMemoryStatus(t.Context(), backend)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401")
}
