//go:build fts5

package db

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Another handle replaces the prepared install on each of the three journaled
// attempts. Later attempts can finish through the ordinary cache path.
func conflictJournaledUsageReads(t *testing.T, database *DB, filter *UsageFilter, timestamp string, afterConflict func(int)) {
	t.Helper()
	other, err := OpenIsolatedContext(t.Context(), database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, other.Close()) })
	reads := 0
	filter.Progress = func(phase string) {
		if phase != "Reading cached daily totals" || reads == 3 {
			return
		}
		reads++
		require.NoError(t, other.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: reads, Role: "assistant", Timestamp: timestamp, Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`)}}))
		_, err := other.GetDailyUsage(t.Context(), UsageFilter{Timezone: filter.Timezone, SkipSessionCounts: true})
		require.NoError(t, err)
		if afterConflict != nil {
			afterConflict(reads)
		}
	}
	t.Cleanup(func() { assert.Equal(t, 3, reads) })
}

func TestUsageJournaledRetryPreservesGroups(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"session", "same-group", "group-b", "group-c"} {
		seedJournaledUsage(t, database, id)
	}
	_, err := database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET group_key=CASE WHEN id IN ('session','same-group') THEN 'group-a' ELSE id END`)
	require.NoError(t, err)
	filter := UsageFilter{Timezone: "UTC", TopSessionsByGroup: true, TopSessionsSort: TopSessionsSortTokens}
	warmJournaledUsage(t, database, filter)
	conflictJournaledUsageReads(t, database, &filter, "2026-08-10T23:31:00Z", nil)
	got, err := database.GetTopSessionsByCost(t.Context(), filter, 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "group-a", got[0].GroupKey)
	assert.Empty(t, got[0].SessionID)
	assert.Equal(t, 350, got[0].InputTokens)
	assert.Empty(t, got[1].GroupKey)
	assert.Equal(t, 200, got[1].InputTokens)
}

func TestUsageJournaledRetryPreservesOffsetDay(t *testing.T) {
	for _, consumer := range []string{"daily", "ranking", "counts"} {
		t.Run(consumer, func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			timestamp := "2026-08-09T07:00:00-04:00"
			require.NoError(t, database.ReplaceSessionMessages(t.Context(), "session", []Message{{SessionID: "session", Ordinal: 0, Role: "assistant", Timestamp: timestamp, Model: "journal-model", TokenUsage: []byte(`{"input_tokens":100}`)}}))
			// This session belongs to the following local day and must stay out
			// of each consumer's result after the final retry.
			seedJournaledUsage(t, database, "outside-window")
			filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "Pacific/Kiritimati"}
			warmJournaledUsage(t, database, filter)
			conflictJournaledUsageReads(t, database, &filter, timestamp, nil)
			switch consumer {
			case "daily":
				got, err := database.GetDailyUsage(t.Context(), filter)
				require.NoError(t, err)
				require.Len(t, got.Daily, 1)
				assert.Equal(t, "2026-08-10", got.Daily[0].Date)
				assert.Equal(t, 250, got.Daily[0].InputTokens)
				assert.Equal(t, 250, got.Totals.InputTokens)
				assert.Equal(t, 1, got.SessionCounts.Total)
			case "ranking":
				got, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
				require.NoError(t, err)
				require.Len(t, got, 1)
				assert.Equal(t, "session", got[0].SessionID)
				assert.Equal(t, 250, got[0].InputTokens)
			case "counts":
				got, err := database.GetUsageSessionCounts(t.Context(), filter)
				require.NoError(t, err)
				assert.Equal(t, UsageSessionCounts{Total: 1, ByProject: map[string]int{"project": 1}, ByAgent: map[string]int{"codex": 1}}, got)
			}
		})
	}
}

func TestUsageJournaledRetryHonorsBulkLane(t *testing.T) {
	for _, consumer := range []string{"daily", "ranking", "counts"} {
		t.Run(consumer, func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{Timezone: "UTC"}
			warmJournaledUsage(t, database, filter)
			var releases []func()
			conflictJournaledUsageReads(t, database, &filter, "2026-08-10T23:31:00Z", func(reads int) {
				if reads == 3 {
					for range bulkReadSlots {
						release, err := database.acquireBulkRead(t.Context())
						require.NoError(t, err)
						release = sync.OnceFunc(release)
						releases = append(releases, release)
						t.Cleanup(release)
					}
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			var err error
			switch consumer {
			case "daily":
				_, err = database.GetDailyUsage(ctx, filter)
			case "ranking":
				_, err = database.GetTopSessionsByCost(ctx, filter, 10)
			case "counts":
				_, err = database.GetUsageSessionCounts(ctx, filter)
			}
			require.ErrorIs(t, err, context.DeadlineExceeded)
			for _, release := range releases {
				release()
			}
			// The same request succeeds once the lane has room again.
			filter.Progress = nil
			switch consumer {
			case "daily":
				got, err := database.GetDailyUsage(t.Context(), filter)
				require.NoError(t, err)
				assert.Equal(t, 250, got.Totals.InputTokens)
			case "ranking":
				got, err := database.GetTopSessionsByCost(t.Context(), filter, 10)
				require.NoError(t, err)
				require.Len(t, got, 1)
				assert.Equal(t, "session", got[0].SessionID)
				assert.Equal(t, 250, got[0].InputTokens)
			case "counts":
				got, err := database.GetUsageSessionCounts(t.Context(), filter)
				require.NoError(t, err)
				assert.Equal(t, UsageSessionCounts{Total: 1, ByProject: map[string]int{"project": 1}, ByAgent: map[string]int{"codex": 1}}, got)
			}
		})
	}
}

func TestUsageJournaledAdmissionCancellation(t *testing.T) {
	for _, test := range []struct {
		name            string
		holderExclusive bool
		waiterExclusive bool
		queuedRetry     bool
	}{
		{name: "shared behind exclusive", holderExclusive: true},
		{name: "exclusive behind shared", waiterExclusive: true},
		{name: "exclusive behind exclusive", holderExclusive: true, waiterExclusive: true},
		{name: "shared behind queued exclusive", queuedRetry: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "session")
			filter := UsageFilter{Timezone: "UTC"}
			warmJournaledUsage(t, database, filter)
			held, resume := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(resume) })
			filter.Progress = func(phase string) {
				if phase == "Reading cached daily totals" {
					close(held)
					<-resume
				}
			}
			holderDone := make(chan error, 1)
			attempt := 1
			if test.holderExclusive {
				attempt = usageFillMaxAttempts + 1
			}
			go func() {
				_, _, _, err := database.queryUsageRollupsOnce(t.Context(), filter, usageQueryKindToken, false, attempt, "")
				holderDone <- err
				close(holderDone)
			}()
			t.Cleanup(func() { unblock(); <-holderDone })
			<-held
			filter.Progress = nil
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var waiters sync.WaitGroup
			t.Cleanup(func() { unblock(); waiters.Wait() })
			start := func(exclusive bool) <-chan error {
				done := make(chan error, 1)
				waiters.Go(func() {
					attempt := 1
					if exclusive {
						attempt = usageFillMaxAttempts + 1
					}
					_, _, _, err := database.queryUsageRollupsOnce(ctx, filter, usageQueryKindToken, false, attempt, "")
					done <- err
				})
				return done
			}
			var retry <-chan error
			if test.queuedRetry {
				retry = start(true)
			}
			waiter := start(test.waiterExclusive)
			cancel()
			for _, done := range []<-chan error{waiter, retry} {
				if done == nil {
					continue
				}
				select {
				case err := <-done:
					require.ErrorIs(t, err, context.Canceled)
				case <-time.After(time.Second):
					require.FailNow(t, "canceled request remained blocked on admission")
				}
			}
			unblock()
			require.NoError(t, <-holderDone)
		})
	}
}

func TestUsageJournaledQueuedAdmissionCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := newUsageCacheManager("")
		defer manager.cancel()
		release, err := manager.admitUsageRead(t.Context(), false)
		require.NoError(t, err)
		defer release()
		retryCtx, cancelRetry := context.WithCancel(t.Context())
		defer cancelRetry()
		retryDone := make(chan error, 1)
		go func() {
			release, err := manager.admitUsageRead(retryCtx, true)
			if release != nil {
				release()
			}
			retryDone <- err
		}()
		synctest.Wait() // The exclusive retry is queued behind the active report.
		readerCtx, cancelReader := context.WithTimeout(t.Context(), time.Second)
		defer cancelReader()
		readerDone := make(chan error, 1)
		go func() {
			release, err := manager.admitUsageRead(readerCtx, false)
			if release != nil {
				release()
			}
			readerDone <- err
		}()
		synctest.Wait() // The queued retry holds off this subsequent reader.
		select {
		case err := <-readerDone:
			require.FailNow(t, "reader bypassed the queued retry", "error: %v", err)
		default:
		}
		require.ErrorIs(t, <-readerDone, context.DeadlineExceeded)
		cancelRetry()
		require.ErrorIs(t, <-retryDone, context.Canceled)
		// Cancellation removed both waiters; another reader can share the holder.
		releaseReader, err := manager.admitUsageRead(t.Context(), false)
		require.NoError(t, err)
		releaseReader()
	})
}
