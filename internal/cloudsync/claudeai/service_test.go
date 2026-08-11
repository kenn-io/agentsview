package claudeai

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/cloudsync/transport"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeBrowserPageHonorsExplicitHasMore(t *testing.T) {
	t.Parallel()
	items, hasMore, err := decodeBrowserPage(json.RawMessage(`{"conversations":[{"uuid":"one"}],"has_more":true}`))
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.True(t, hasMore, "a short page must continue when Claude explicitly sets has_more")

	_, hasMore, err = decodeBrowserPage(json.RawMessage(`{"conversations":[{"uuid":"one"}],"has_more":false}`))
	require.NoError(t, err)
	assert.False(t, hasMore)
}

func TestServiceRunsBeyondStartRequestContext(t *testing.T) {
	t.Parallel()
	broker := transport.NewBroker()
	service := NewService(broker, nil, t.TempDir(), "local")
	t.Cleanup(service.Close)
	requestContext, cancel := context.WithCancel(context.Background())
	_, err := service.Start(requestContext, SyncIncremental)
	require.NoError(t, err)
	cancel()

	claimContext, claimCancel := context.WithTimeout(context.Background(), time.Second)
	defer claimCancel()
	request, err := broker.Claim(claimContext)
	require.NoError(t, err)
	require.NoError(t, broker.Complete(transport.Response{ID: request.ID, Lease: request.Lease, Status: 200, Body: json.RawMessage(`{"conversations":[],"has_more":false}`)}))
	require.Eventually(t, func() bool { return service.Status().Status == "completed" }, time.Second, 10*time.Millisecond)
}

func TestSchedulePersistsAndCloseStopsActiveJob(t *testing.T) {
	root := t.TempDir()
	service := NewService(transport.NewBroker(), nil, root, "local")
	t.Cleanup(service.Close)
	require.NoError(t, service.ConfigureSchedule(ScheduleConfig{Enabled: true, IntervalMinutes: 60}))
	reloaded := NewService(transport.NewBroker(), nil, root, "local")
	t.Cleanup(reloaded.Close)
	assert.True(t, reloaded.Schedule().Enabled)
	assert.Equal(t, 60, reloaded.Schedule().IntervalMinutes)
	_, err := service.Start(context.Background(), SyncIncremental)
	require.NoError(t, err)
	service.Close()
	require.Eventually(t, func() bool { return service.Status().Status == "cancelled" }, time.Second, 10*time.Millisecond)
}

func TestDecodeBrowserPageFallsBackWhenHasMoreIsAbsent(t *testing.T) {
	t.Parallel()
	_, hasMore, err := decodeBrowserPage(json.RawMessage(`{"conversations":[{"uuid":"one"}]}`))
	require.NoError(t, err)
	assert.True(t, hasMore)
}
