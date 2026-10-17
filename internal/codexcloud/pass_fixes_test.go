package codexcloud

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFailedFirstFetchSendsListedCreationTime(t *testing.T) {
	env := newUsageEnv(t)
	thread := finishedThread("a", ago(time.Hour), message("m", "hi"))
	env.threads.Set(thread)
	fail, mu := true, &sync.Mutex{}
	env.failThread("a", &fail, mu)

	env.run(t)

	require.Contains(t, env.account(t).Retry, "a", "the history read failed")
	requests := env.usage.Requests()
	require.Len(t, requests, 1)
	require.Len(t, requests[0].Threads, 1)
	assert.Equal(t, "a", requests[0].Threads[0].ThreadID)
	assert.Equal(t, time.Unix(thread.CreatedAt, 0).UTC().Format(time.RFC3339), requests[0].Threads[0].CreatedAt,
		"the chat's listed creation time, not the Unix epoch")
}

func TestHomeWaitingOnALiveRefreshRequestShowsItsRow(t *testing.T) {
	env := newMonitorEnv(t)
	env.threads.Set(runningThread("a", "warm"))
	env.join(t, "a")
	require.NoError(t, env.host.ServerRequestThread(t.Context(), "a", refreshMethod, map[string]any{"reason": "unauthorized"}))
	env.waitLeft(t, "a")
	home := env.homes[0]
	require.False(t, env.syncer.Waits.HomeReady(home, env.login().AuthModTime), "live capture recorded the wait")

	env.run(t)

	row, ok := env.syncer.Status.Row(home)
	require.True(t, ok, "the waiting home has a status row")
	assert.Equal(t, StateError, row.State)
	assert.Equal(t, Message(FailRefreshRequested, time.Time{}), row.Message)
}
