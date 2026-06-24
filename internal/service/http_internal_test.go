package service

import (
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
