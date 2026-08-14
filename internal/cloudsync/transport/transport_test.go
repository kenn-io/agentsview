package transport

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrokerDeliversSingleLeasedResponse(t *testing.T) {
	broker := NewBroker()
	result := make(chan Response, 1)
	go func() {
		response, err := broker.Do(context.Background(), "provider", "list", map[string]int{"offset": 0})
		require.NoError(t, err)
		result <- response
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := broker.Claim(ctx)
	require.NoError(t, err)
	require.NoError(t, broker.Complete(Response{ID: request.ID, Lease: request.Lease, Status: 200, Body: []byte(`[]`)}))
	assert.Equal(t, 200, (<-result).Status)
	assert.Error(t, broker.Complete(Response{ID: request.ID, Lease: request.Lease, Status: 200}))
}

func TestBrokerRequeuesExpiredLease(t *testing.T) {
	previous := leaseTimeout
	leaseTimeout = 10 * time.Millisecond
	t.Cleanup(func() { leaseTimeout = previous })
	broker := NewBroker()
	result := make(chan error, 1)
	go func() { _, err := broker.Do(context.Background(), "provider", "detail", struct{}{}); result <- err }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, err := broker.Claim(ctx)
	require.NoError(t, err)
	second, err := broker.Claim(ctx)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	assert.NotEqual(t, first.Lease, second.Lease)
	require.NoError(t, broker.Complete(Response{ID: second.ID, Lease: second.Lease, Status: 200, Body: []byte(`[]`)}))
	assert.NoError(t, <-result)
}

func TestBrokerRejectsInvalidLease(t *testing.T) {
	broker := NewBroker()
	go func() { _, _ = broker.Do(context.Background(), "provider", "detail", struct{}{}) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := broker.Claim(ctx)
	require.NoError(t, err)
	assert.Error(t, broker.Complete(Response{ID: request.ID, Lease: "wrong"}))
}
