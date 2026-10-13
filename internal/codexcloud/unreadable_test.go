package codexcloud

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
)

// retryUntil runs a pass at each retry deadline of id until the clock
// reaches limit or the thread leaves the retry set.
func (e *passEnv) retryUntil(t *testing.T, id string, limit time.Time) {
	t.Helper()
	for {
		retry, ok := e.account(t).Retry[id]
		if !ok || retry.NextAttempt.After(limit) {
			return
		}
		e.clock.Set(retry.NextAttempt)
		e.run(t)
	}
}

func TestUnreadableAfterSevenDays(t *testing.T) {
	cases := map[string]func(t *testing.T) *passEnv{
		"always fails": func(t *testing.T) *passEnv {
			t.Helper()
			env := newPassEnv(t)
			env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
			fail, mu := true, &sync.Mutex{}
			env.failThread("a", &fail, mu)
			return env
		},
		"one item over the read limit": func(t *testing.T) *passEnv {
			t.Helper()
			env := newPassEnv(t)
			env.syncer.Dial = smallLimitDial
			env.threads.Set(finishedThread("a", ago(time.Hour), bigItem("h", 70<<10)))
			return env
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			env := setup(t)
			env.run(t)
			first := env.account(t).Retry["a"].FirstFailure
			require.Equal(t, fetchNow, first)

			env.retryUntil(t, "a", first.Add(7*24*time.Hour-time.Minute))
			require.Contains(t, env.account(t).Retry, "a", "a thread is retried for 7 days")

			env.retryUntil(t, "a", first.Add(8*24*time.Hour))
			acct := env.account(t)
			assert.NotContains(t, acct.Retry, "a")
			assert.NotContains(t, acct.Unfinished, "a")
			assert.NotContains(t, acct.CheckAgain, "a")
			st, err := env.syncer.Store.LoadThread(passKey, "a")
			require.NoError(t, err)
			assert.Equal(t, ago(time.Hour), st.UpdatedAt, "the listing updatedAt is stored")
			row, ok := env.syncer.Status.Row(string(passKey))
			require.True(t, ok)
			assert.NotEqual(t, StateRetrying, row.State, "the thread no longer counts as not imported")

			env.clock.Advance(2 * time.Hour)
			from := len(env.host.Requests())
			env.run(t)
			assert.NotContains(t, env.fetchedThreads(from), "a", "an unreadable thread is not fetched again")

			env.threads.Update("a", func(th *codexcloudtest.Thread) { th.UpdatedAt = env.clock.Now().Unix() - 60 })
			env.clock.Advance(time.Minute)
			from = len(env.host.Requests())
			env.run(t)
			assert.Contains(t, env.fetchedThreads(from), "a", "a new updatedAt selects the thread again")
			assert.Contains(t, env.account(t).Retry, "a")
		})
	}
}

func TestNewUpdatedAtRestartsUnreadableCount(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "hi")))
	fail, mu := true, &sync.Mutex{}
	env.failThread("a", &fail, mu)
	env.run(t)

	env.retryUntil(t, "a", fetchNow.Add(5*24*time.Hour))
	restart := env.clock.Now().Add(time.Minute)
	env.threads.Update("a", func(th *codexcloudtest.Thread) { th.UpdatedAt = restart.Unix() - 30 })
	env.clock.Set(restart)
	// The thread waits out its backoff; the listing still sees the change.
	env.run(t)
	assert.Equal(t, restart, env.account(t).Retry["a"].FirstFailure, "a new updatedAt restarts the 7 days")

	env.retryUntil(t, "a", fetchNow.Add(9*24*time.Hour))
	assert.Contains(t, env.account(t).Retry, "a", "7 days have not passed since the new updatedAt")

	env.retryUntil(t, "a", restart.Add(8*24*time.Hour))
	assert.NotContains(t, env.account(t).Retry, "a")
}
