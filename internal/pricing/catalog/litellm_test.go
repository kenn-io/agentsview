package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFetchLiteLLMPricingHonorsCanceledContext(t *testing.T) {
	var requested atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		requested.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetchLiteLLMPricing(ctx, server.Client(), server.URL)

	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, requested.Load())
}
