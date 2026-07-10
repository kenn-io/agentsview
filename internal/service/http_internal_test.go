package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHTTPBackendUsesLongRunningClient(t *testing.T) {
	t.Parallel()
	svc := NewHTTPBackend("http://example.test", "", false)
	backend, ok := svc.(*httpBackend)
	require.True(t, ok)
	require.NotNil(t, backend.client)
	require.NotNil(t, backend.longRunningClient)

	assert.Equal(t, 30*time.Second, backend.client.Timeout)
	assert.Zero(t, backend.longRunningClient.Timeout)
}

func TestSearchContentUsesLongRunningClient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"matches":[]}`))
	}))
	t.Cleanup(srv.Close)

	svc := NewHTTPBackend(srv.URL, "", false)
	backend, ok := svc.(*httpBackend)
	require.True(t, ok)
	backend.client.Timeout = 10 * time.Millisecond

	result, err := svc.SearchContent(context.Background(), ContentSearchRequest{
		Pattern: "slow first query",
		Mode:    "semantic",
	})
	require.NoError(t, err)
	assert.Empty(t, result.Matches)
}
