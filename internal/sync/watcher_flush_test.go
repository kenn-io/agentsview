package sync

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatcherFlushPendingWaitsForLocalSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan WatchBatch, 4)
		releases := make(chan struct{})
		backend := newFakeWatchBackend()
		w, err := newWatcherWithBackend(time.Hour, time.Hour, func(ctx context.Context, batch WatchBatch) error {
			calls <- batch
			select {
			case <-releases:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, backend, 8, 1000)
		require.NoError(t, err)
		w.Start()
		defer w.Stop()
		w.eventSink.Add(backendEvent{Path: "/first", Op: backendOpWrite})
		w.eventSink.MarkImmediate()
		w.eventSink.signal()
		synctest.Wait()
		require.Len(t, calls, 1)
		assert.Equal(t, []string{"/first"}, (<-calls).Paths)
		backend.events <- backendEvent{Path: "/queued", Op: backendOpWrite}
		flushed := make(chan error, 1)
		go func() { flushed <- w.FlushPending(t.Context()) }()
		synctest.Wait()
		assert.Empty(t, flushed, "the running local write must finish first")
		backend.events <- backendEvent{Path: "/later", Op: backendOpWrite}
		releases <- struct{}{}
		synctest.Wait()
		require.Len(t, calls, 1)
		assert.Equal(t, []string{"/queued"}, (<-calls).Paths)
		assert.Empty(t, flushed, "the handoff waits for its fresh batch")
		releases <- struct{}{}
		synctest.Wait()
		require.Len(t, flushed, 1)
		require.NoError(t, <-flushed)
		assert.False(t, w.eventSink.PendingEmpty(), "later arrivals belong to the next batch")
	})
}

func TestWatcherFlushPendingIncludesReadyBackendDelivery(t *testing.T) {
	for _, capacity := range []int{0, 4} {
		// Both the backend delivery and flush request can be ready at the
		// select. Exercise either selection order, including an unbuffered
		// sender as used by the fsnotify backend on Linux and Windows.
		for range 32 {
			synctest.Test(t, func(t *testing.T) {
				backend := newFakeWatchBackend()
				backend.events = make(chan backendEvent, capacity)
				paths := []string{"/ready"}
				if capacity > 0 {
					paths = []string{"/first", "/second", "/third"}
				}
				go func() {
					for _, path := range paths {
						select {
						case backend.events <- backendEvent{Path: path, Op: backendOpWrite}:
						case <-backend.stopped:
							return
						}
					}
				}()
				synctest.Wait()
				calls := make(chan WatchBatch, 1)
				w, err := newWatcherWithBackend(time.Hour, time.Hour, func(_ context.Context, batch WatchBatch) error {
					calls <- batch
					return nil
				}, backend, 8, 1000)
				require.NoError(t, err)
				w.Start()
				defer w.Stop()
				require.NoError(t, w.FlushPending(t.Context()))
				require.Len(t, calls, 1, "the handoff must finish already-ready backend work")
				assert.ElementsMatch(t, paths, (<-calls).Paths)
			})
			if t.Failed() {
				return
			}
		}
	}
}

func TestWatcherFlushPendingKeepsRetryBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failed := errors.New("source unavailable")
		calls := make(chan WatchBatch, 4)
		w, err := newWatcherWithBackend(time.Hour, time.Hour, func(_ context.Context, batch WatchBatch) error {
			calls <- batch
			if len(batch.ReconcileRoots) > 0 {
				return failed
			}
			return nil
		}, newFakeWatchBackend(), 8, 1000)
		require.NoError(t, err)
		w.Start()
		defer w.Stop()
		w.eventSink.Add(backendEvent{Path: "/retry", Root: "/retry", Op: backendOpReconcileRootChange})
		require.ErrorIs(t, w.FlushPending(t.Context()), failed)
		require.Len(t, calls, 1)
		<-calls
		w.eventSink.Add(backendEvent{Path: "/fresh", Op: backendOpWrite})
		require.NoError(t, w.FlushPending(t.Context()))
		require.Len(t, calls, 1)
		assert.Equal(t, []string{"/fresh"}, (<-calls).Paths)
		assert.False(t, w.eventSink.RetryEmpty())
		require.NoError(t, w.FlushPending(t.Context()))
		assert.Empty(t, calls)
	})
}

func TestWatcherFlushPendingStopAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		w, err := newWatcherWithBackend(time.Hour, time.Hour, func(ctx context.Context, _ WatchBatch) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}, newFakeWatchBackend(), 8, 1000)
		require.NoError(t, err)
		cancelCallback := w.callbackCancel
		w.callbackCancel = func() {
			cancelCallback()
			// Allow the worker and queued flush to run as soon as the callback
			// is canceled, before Stop continues with its remaining cleanup.
			synctest.Wait()
		}
		w.Start()
		w.eventSink.Add(backendEvent{Path: "/file", Op: backendOpWrite})
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() { result <- w.FlushPending(ctx) }()
		<-started
		cancel()
		require.ErrorIs(t, <-result, context.Canceled)
		stopped := make(chan error, 1)
		go func() { stopped <- w.FlushPending(t.Context()) }()
		synctest.Wait()
		w.Stop()
		require.Error(t, <-stopped)
		require.Error(t, w.FlushPending(t.Context()))
	})
}
