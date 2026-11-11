package sync

import (
	"context"
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
				override.Set(tt.shorter)
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

		override.Set(time.Second)
		synctest.Wait()
		require.Len(t, calls, 2, "a shorter override must release the waiting sync")
		assert.Equal(t, 2*time.Second, calls[1].Sub(start))

		override.Set(0)
		time.Sleep(100 * time.Millisecond)
		backend.sendEvent(t, "/sessions/active.jsonl")
		time.Sleep(10 * time.Second)
		synctest.Wait()
		require.Len(t, calls, 3)
		assert.Equal(t, 7*time.Second, calls[2].Sub(start),
			"removing the override must restore the configured interval")
	})
}
