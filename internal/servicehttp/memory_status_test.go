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

func TestMemoryStatusOlderServerIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	backend := servicehttp.NewHTTPBackend(server.URL, "", true, "")

	status, err := service.GetMemoryStatus(t.Context(), backend)
	require.NoError(t, err)
	assert.Equal(t, service.MemoryUnknown, status.Status)
	assert.Equal(t, "unsupported", status.Lexical.Reason)
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
