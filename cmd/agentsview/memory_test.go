package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemorySessionStartUsesBoundedContext(t *testing.T) {
	original := runMemorySessionStart
	t.Cleanup(func() { runMemorySessionStart = original })

	var called bool
	runMemorySessionStart = func(ctx context.Context) error {
		called = true
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		remaining := time.Until(deadline)
		assert.Positive(t, remaining)
		assert.LessOrEqual(t, remaining, memorySessionStartTimeout)
		return nil
	}

	_, err := executeCommand(newRootCommand(), "memory", "session-start")
	require.NoError(t, err)
	assert.True(t, called)
}

func TestMemorySessionStartDisableSwitchSkipsRequest(t *testing.T) {
	original := runMemorySessionStart
	t.Cleanup(func() { runMemorySessionStart = original })
	t.Setenv("AGENTSVIEW_DISABLE_AUTO_SYNC", "1")

	runMemorySessionStart = func(context.Context) error {
		require.Fail(t, "disabled automatic sync must not request a refresh")
		return nil
	}

	_, err := executeCommand(newRootCommand(), "memory", "session-start")
	require.NoError(t, err)
}

func TestMemoryRefreshSchedulerCoalescesBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		queue := newMemoryRefreshQueue()
		var runs atomic.Int32
		go runMemoryRefreshScheduler(ctx, queue.requests, time.Second, func() {
			runs.Add(1)
		})

		queue.Notify()
		queue.Notify()
		queue.Notify()
		time.Sleep(time.Second)
		synctest.Wait()

		assert.Equal(t, int32(1), runs.Load())
	})
}

func TestPostMemoryRefreshTargetsAuthenticatedQueueRoute(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/memory-base/api/v1/memory/refresh", r.URL.Path)
		assert.Equal(t, serverURL, r.Header.Get("Origin"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusAccepted)
	}))
	serverURL = server.URL
	t.Cleanup(server.Close)

	require.NoError(t, postMemoryRefresh(
		t.Context(), server.URL+"/memory-base/", "test-token",
	))
}
