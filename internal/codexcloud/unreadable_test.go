package codexcloud

import (
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
)

// retryAt jumps to a due retry, leaving backoff cadence to
// TestFailingThreadBacksOffAndDoesNotHoldWatermark.
func (e *passEnv) retryAt(t *testing.T, id string, when time.Time) {
	t.Helper()
	retries := e.account(t).Retry
	require.Contains(t, retries, id)
	require.False(t, retries[id].NextAttempt.After(when), "the retry must be due")
	e.clock.Set(when)
	e.run(t)
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

			env.retryAt(t, "a", first.Add(7*24*time.Hour-time.Minute))
			require.Contains(t, env.account(t).Retry, "a", "a thread is retried for 7 days")

			env.retryAt(t, "a", env.account(t).Retry["a"].NextAttempt)
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
	cases := []struct {
		name       string
		checkAgain bool
		pastOld    time.Duration
	}{
		{name: "listing row", pastOld: 9 * 24 * time.Hour},
		{name: "retry without listing", checkAgain: true, pastOld: 7*24*time.Hour + 2*time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPassEnv(t)
			updatedAgo := time.Hour
			if tc.checkAgain {
				updatedAgo = 30 * time.Second
			}
			env.threads.Set(finishedThread("a", ago(updatedAgo), message("m", "hi")))
			if tc.checkAgain {
				env.run(t)
				require.True(t, env.account(t).CheckAgain["a"])
			}
			fail, mu := true, &sync.Mutex{}
			env.failThread("a", &fail, mu)
			if tc.checkAgain {
				// The thread fails when check-again selects it, with no listing row.
				env.clock.Advance(5 * time.Minute)
			}
			env.run(t)
			first := env.account(t).Retry["a"].FirstFailure
			if tc.checkAgain {
				require.Equal(t, env.clock.Now(), first)
			}

			env.retryAt(t, "a", first.Add(5*24*time.Hour))
			restart := env.clock.Now().Add(time.Minute)
			env.threads.Update("a", func(th *codexcloudtest.Thread) { th.UpdatedAt = restart.Unix() - 30 })
			env.clock.Set(restart)
			// The thread waits out its backoff; the listing still sees the change.
			env.run(t)
			assert.Equal(t, restart, env.account(t).Retry["a"].FirstFailure, "a new updatedAt restarts the 7 days")

			env.retryAt(t, "a", first.Add(tc.pastOld))
			assert.Contains(t, env.account(t).Retry, "a", "7 days have not passed since the new updatedAt")

			env.retryAt(t, "a", restart.Add(7*24*time.Hour-time.Minute))
			require.Contains(t, env.account(t).Retry, "a", "a changed thread is retried for another 7 days")
			env.retryAt(t, "a", env.account(t).Retry["a"].NextAttempt)
			assert.NotContains(t, env.account(t).Retry, "a")
		})
	}
}

func TestRetiredActiveThreadStaysRetiredUntilItChanges(t *testing.T) {
	env := newPassEnv(t)
	th := finishedThread("a", ago(time.Hour), message("m", "hi"))
	th.Status = "active"
	env.threads.Set(th)
	fail, mu := true, &sync.Mutex{}
	env.failThread("a", &fail, mu)
	var logged strings.Builder
	env.syncer.Log = log.New(&logged, "", 0)
	env.run(t)
	env.retryAt(t, "a", fetchNow.Add(7*24*time.Hour))
	require.NotContains(t, env.account(t).Retry, "a", "retired after 7 days")
	assert.Contains(t, logged.String(), "chat a: stopped retrying after failing since")
	assert.Contains(t, logged.String(), "Its next change in OpenAI starts the retries again.")

	env.clock.Advance(time.Hour)
	from := len(env.host.Requests())
	env.run(t)
	assert.NotContains(t, env.fetchedThreads(from), "a", "listed active but unchanged since it was retired")
	assert.NotContains(t, env.account(t).Retry, "a")

	mu.Lock()
	fail = false
	mu.Unlock()
	env.threads.Update("a", func(th *codexcloudtest.Thread) { th.UpdatedAt = env.clock.Now().Unix() - 3600 })
	env.clock.Advance(time.Minute)
	env.run(t)
	env.clock.Advance(5 * time.Minute)
	from = len(env.host.Requests())
	env.run(t)
	assert.Contains(t, env.fetchedThreads(from), "a", "after a change, an active thread is fetched every pass again")
}
