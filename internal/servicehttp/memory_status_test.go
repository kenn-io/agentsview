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
	ready := service.MemoryStatus{
		Status:   service.MemoryReady,
		Archive:  service.MemoryArchiveStatus{Backend: "sqlite", ReadOnly: true},
		Lexical:  service.MemoryCapabilityStatus{Status: service.MemoryReady},
		Semantic: service.MemoryVectorStatus{Status: service.MemoryReady, Generation: "generation-a", Embedded: 7},
		Sources:  service.MemorySourceStatus{Status: service.MemoryReady},
	}
	for _, tc := range []struct {
		name       string
		httpStatus int
		want       service.MemoryReadinessState
		wantReason string
		wantError  string
	}{
		{"older server", http.StatusNotFound, service.MemoryUnknown, "unsupported", ""},
		{"ready", http.StatusOK, service.MemoryReady, "", ""},
		{"unauthorized", http.StatusUnauthorized, "", "", "HTTP 401"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/memory/status", r.URL.Path)
				if tc.httpStatus != http.StatusOK {
					http.Error(w, http.StatusText(tc.httpStatus), tc.httpStatus)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, ready))
			}))
			t.Cleanup(server.Close)
			backend := servicehttp.NewHTTPBackend(server.URL, "", true, "")
			status, err := service.GetMemoryStatus(t.Context(), backend)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			if tc.httpStatus == http.StatusOK {
				assert.Equal(t, ready, status)
				return
			}
			assert.Equal(t, tc.want, status.Status)
			assert.Equal(t, tc.wantReason, status.Lexical.Reason)
		})
	}
}
