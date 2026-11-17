package sync

import (
	"context"
	"slices"
	gosync "sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatcherIntervalOverride(t *testing.T) {
	const batchDelay = 500 * time.Millisecond
	for _, tt := range []struct {
		name                string
		configured, shorter time.Duration
		want                []time.Duration
	}{
		{
			name:       "override shortens the configured interval",
			configured: 5 * time.Second, shorter: time.Second,
			want: []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond, 2500 * time.Millisecond},
		},
		{
			name:       "shorter configured interval wins",
			configured: 300 * time.Millisecond, shorter: time.Second,
			want: []time.Duration{
				500 * time.Millisecond, 1000 * time.Millisecond,
				1500 * time.Millisecond, 2000 * time.Millisecond, 2500 * time.Millisecond,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				override := &IntervalOverride{}
				t.Cleanup(override.Hold(tt.shorter))
				backend := newFakeWatchBackend()
				var calls []time.Time
				w, err := newWatcherWithBackendOptions(
					batchDelay, tt.configured,
					func(context.Context, WatchBatch) error {
						calls = append(calls, time.Now())
						return nil
					},
					backend, 8, 1_000,
					WatcherOptions{MinIntervalOverride: override},
				)
				require.NoError(t, err)
				w.Start()
				t.Cleanup(w.Stop)

				start := time.Now()
				for time.Since(start) < 2600*time.Millisecond {
					// Settle a due batch timer before delivering an event at the
					// same instant, so it belongs to the next batch.
					synctest.Wait()
					backend.sendEvent(t, "/sessions/active.jsonl")
					synctest.Wait()
					time.Sleep(100 * time.Millisecond)
				}
				synctest.Wait()

				got := make([]time.Duration, 0, len(calls))
				for _, at := range calls {
					got = append(got, at.Sub(start))
				}
				assert.Equal(t, tt.want, got)
			})
		})
	}
}

func TestWatcherIntervalOverrideChangeReschedules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			batchDelay = 500 * time.Millisecond
			configured = 5 * time.Second
		)
		override := &IntervalOverride{}
		backend := newFakeWatchBackend()
		var calls []time.Time
		w, err := newWatcherWithBackendOptions(
			batchDelay, configured,
			func(context.Context, WatchBatch) error {
				calls = append(calls, time.Now())
				return nil
			},
			backend, 8, 1_000,
			WatcherOptions{MinIntervalOverride: override},
		)
		require.NoError(t, err)
		w.Start()
		t.Cleanup(w.Stop)

		start := time.Now()
		backend.sendEvent(t, "/sessions/active.jsonl")
		time.Sleep(time.Second)
		// This sync waits for the configured interval: due at 5.5 s.
		backend.sendEvent(t, "/sessions/active.jsonl")
		time.Sleep(time.Second)
		synctest.Wait()
		require.Len(t, calls, 1)

		releaseSlow := override.Hold(3 * time.Second)
		release := override.Hold(time.Second)
		synctest.Wait()
		require.Len(t, calls, 2, "the shortest hold must release the waiting sync")
		assert.Equal(t, 2*time.Second, calls[1].Sub(start))

		release()
		releaseSlow()
		time.Sleep(100 * time.Millisecond)
		backend.sendEvent(t, "/sessions/active.jsonl")
		time.Sleep(10 * time.Second)
		synctest.Wait()
		require.Len(t, calls, 3)
		assert.Equal(t, 7*time.Second, calls[2].Sub(start),
			"removing the override must restore the configured interval")
	})
}

// A watcher with a long interval for when nobody is waiting must still retry
// a failed sync at the pace of a client that is waiting.
func TestWatcherRetryFollowsWaitingClientPace(t *testing.T) {
	const (
		background = 10 * time.Minute
		live       = 5 * time.Second
	)
	for _, tt := range []struct {
		name         string
		connectAfter time.Duration
		want         time.Duration
	}{
		{name: "client connected before the failure", connectAfter: 0, want: live},
		{name: "client connects while the retry waits", connectAfter: time.Minute, want: time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				override := &IntervalOverride{}
				backend := newFakeWatchBackend()
				// The callback runs on the watcher's goroutine and the test
				// reads between syncs, so the record needs a lock.
				var mu gosync.Mutex
				var calls []time.Time
				recorded := func() []time.Time {
					mu.Lock()
					defer mu.Unlock()
					return slices.Clone(calls)
				}
				w, err := newWatcherWithBackendOptions(
					0, background,
					func(context.Context, WatchBatch) error {
						mu.Lock()
						calls = append(calls, time.Now())
						first := len(calls) == 1
						mu.Unlock()
						if first {
							return retryScopedWatchError{retry: WatchBatch{
								ReconcileRoots: []string{"/sessions"},
							}}
						}
						return nil
					},
					backend, 8, 1_000,
					WatcherOptions{MinIntervalOverride: override, RetryInterval: live},
				)
				require.NoError(t, err)
				w.Start()
				t.Cleanup(w.Stop)
				if tt.connectAfter == 0 {
					t.Cleanup(override.Hold(live))
				}

				backend.sendBackendEvent(t, backendEvent{
					Path: "/sessions", Root: "/sessions", Op: backendOpReconcileRootChange,
				})
				synctest.Wait()
				require.Len(t, recorded(), 1)
				if tt.connectAfter > 0 {
					time.Sleep(tt.connectAfter)
					synctest.Wait()
					require.Len(t, recorded(), 1, "nobody is waiting for the retry yet")
					t.Cleanup(override.Hold(live))
				}
				time.Sleep(time.Minute)
				synctest.Wait()

				got := recorded()
				require.Len(t, got, 2, "the retry needs no new file event")
				assert.Equal(t, tt.want, got[1].Sub(got[0]))
			})
		})
	}
}

func TestIntervalOverrideUsesShortestHold(t *testing.T) {
	override := &IntervalOverride{}
	require.Zero(t, override.Value())

	releaseUI := override.Hold(5 * time.Second)
	releaseStream := override.Hold(time.Second)
	assert.Equal(t, time.Second, override.Value())

	releaseStream()
	releaseStream()
	assert.Equal(t, 5*time.Second, override.Value(),
		"releasing one hold twice must not release another")

	releaseSecondUI := override.Hold(5 * time.Second)
	releaseUI()
	assert.Equal(t, 5*time.Second, override.Value(),
		"an equal hold from another holder remains")

	releaseSecondUI()
	assert.Zero(t, override.Value())
	assert.NotPanics(t, override.Hold(0), "a hold of zero holds nothing")
	assert.Zero(t, override.Value())
}
