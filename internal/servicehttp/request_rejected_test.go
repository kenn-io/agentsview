package servicehttp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestIsRequestRejectedClassifiesDaemonAnswers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "invalid filter", status: http.StatusBadRequest, body: `{"error":"bad limit"}`, want: true},
		{name: "daemon unavailable", status: http.StatusServiceUnavailable, body: `{"error":"busy"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/sessions/s1/messages", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			backend := servicehttp.NewHTTPBackend(srv.URL, "", false, "")

			_, err := backend.Messages(t.Context(), "s1", service.MessageFilter{})
			require.Error(t, err)
			assert.Equal(t, tt.want, servicehttp.IsRequestRejected(err), err)
		})
	}
}
