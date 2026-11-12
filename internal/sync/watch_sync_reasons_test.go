package sync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatcherFullSyncLogsCauseWithoutPendingOverflow(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reasons     WatchSyncReasons
		wantReasons WatchSyncReasons
		wantLabels  string
	}{
		{name: "backend event loss", wantReasons: watchReasonBackendEventLoss, wantLabels: "backend_event_loss"},
		{name: "fallback", reasons: watchReasonFallbackStarted | watchReasonStreamStart, wantReasons: watchReasonFallbackStarted | watchReasonStreamStart, wantLabels: "fallback_started,stream_start_failed"},
		{name: "native recovery", reasons: watchReasonNativeRecovery, wantReasons: watchReasonNativeRecovery, wantLabels: "native_recovery"},
		{name: "fallback buffer", reasons: watchReasonFallbackBuffer, wantReasons: watchReasonFallbackBuffer, wantLabels: "fallback_buffer_overflow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureOperationLogs(t)
			backend := newFakeWatchBackend()
			calls := make(chan WatchBatch, 1)
			watcher, err := newWatcherWithBackend(0, 0, func(_ context.Context, batch WatchBatch) error {
				calls <- batch
				return nil
			}, backend, 8, 1_000)
			require.NoError(t, err)
			watcher.Start()
			t.Cleanup(watcher.Stop)
			backend.sendBackendEvent(t, backendEvent{Op: backendOpFullSync, Reasons: tc.reasons})
			assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true, Reasons: tc.wantReasons}, receiveWatchBatch(t, calls))
			var fullSyncLogs []string
			for line := range strings.SplitSeq(logs.take(), "\n") {
				if strings.HasPrefix(line, "watcher: authoritative full sync requested") {
					fullSyncLogs = append(fullSyncLogs, line)
				}
			}
			assert.Equal(t, []string{
				"watcher: authoritative full sync requested (pending overflows=0 lost_events=true reasons=" + tc.wantLabels + ")",
			}, fullSyncLogs)
		})
	}
}

func TestWatchReasonsSurviveOverflowAndNativeHandoff(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entries    int
		bytes      int
		wantReason WatchSyncReasons
	}{
		{name: "entries", entries: 1, bytes: 1000, wantReason: watchReasonPendingEntries},
		{name: "bytes", entries: 8, bytes: len("/one"), wantReason: watchReasonPendingBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := newWatchEventSink(tc.entries, tc.bytes)
			sink.Add(backendEvent{Path: "/one", Op: backendOpWrite})
			sink.Add(backendEvent{Path: "/two", Op: backendOpWrite})
			sink.RetainAuthoritative(backendLifecycleToken{}, watchReasonNativeRecovery)
			batch, ok := sink.Take(nil)
			require.True(t, ok)
			assert.Equal(t, WatchBatch{
				FullSync: true, LostEvents: true, Reasons: tc.wantReason | watchReasonNativeRecovery,
			}, batch)
			assert.Equal(t, uint32(1), sink.overflows.Load())
			sink.Add(backendEvent{Path: "/new", Op: backendOpWrite})
			batch, ok = sink.Take(nil)
			require.True(t, ok)
			assert.Equal(t, WatchBatch{Paths: []string{"/new"}, Renames: []WatchRename{}, ReconcileRoots: []string{}}, batch)
		})
	}
}

func TestWatchAccumulatorMergesReasonsAfterFullPromotion(t *testing.T) {
	accumulator := NewWatchBatchAccumulator(nil)
	accumulator.Add(WatchBatch{FullSync: true, LostEvents: true, Reasons: watchReasonFallbackStarted})
	accumulator.Add(WatchBatch{FullSync: true, LostEvents: true, Reasons: watchReasonNativeRecovery})
	batch, ok := accumulator.Take()
	require.True(t, ok)
	assert.Equal(t, WatchBatch{
		FullSync: true, LostEvents: true, Reasons: watchReasonFallbackStarted | watchReasonNativeRecovery,
	}, batch)
	accumulator.Add(WatchBatch{Paths: []string{"/sessions/new.jsonl"}})
	batch, ok = accumulator.Take()
	require.True(t, ok)
	assert.Equal(t, WatchBatch{Paths: []string{"/sessions/new.jsonl"}, Renames: []WatchRename{}, ReconcileRoots: []string{}}, batch)
}

func TestWatchReasonsSurviveNarrowedRetry(t *testing.T) {
	backend := newFakeWatchBackend()
	calls := make(chan WatchBatch, 2)
	attempt := 0
	watcher, err := newWatcherWithBackend(0, 0, func(_ context.Context, batch WatchBatch) error {
		attempt++
		calls <- batch
		if attempt == 1 {
			return &watchBatchApplyError{
				cause: errors.New("retry this scope"),
				retry: WatchBatch{ReconcileRoots: []string{"/sessions"}, LostEvents: true},
			}
		}
		return nil
	}, backend, 8, 1_000)
	require.NoError(t, err)
	watcher.Start()
	t.Cleanup(watcher.Stop)
	backend.sendBackendEvent(t, backendEvent{Op: backendOpFullSync, Reasons: watchReasonNativeRecovery})
	assert.Equal(t, WatchBatch{FullSync: true, LostEvents: true, Reasons: watchReasonNativeRecovery}, receiveWatchBatch(t, calls))
	assert.Equal(t, WatchBatch{
		Paths: []string{}, Renames: []WatchRename{}, ReconcileRoots: []string{"/sessions"}, LostEvents: true,
		Reasons: watchReasonNativeRecovery | watchReasonCallbackRetry,
	}, receiveWatchBatch(t, calls))
}

func TestWatchRetryReasonsPreserveErrorIdentity(t *testing.T) {
	sentinel := errors.New("scope unavailable")
	original := &watchBatchApplyError{cause: sentinel, retry: WatchBatch{
		ReconcileRoots: []string{"/sessions"}, Reasons: watchReasonNativeRecovery,
	}}
	err := watchBatchErrorWithReasons(original, watchReasonCallbackRetry)
	require.ErrorIs(t, err, sentinel)
	retry, ok := callbackRetryBatch(err)
	require.True(t, ok)
	assert.Equal(t, WatchBatch{
		ReconcileRoots: []string{"/sessions"}, Reasons: watchReasonNativeRecovery | watchReasonCallbackRetry,
	}, retry)
	assert.Equal(t, WatchBatch{
		ReconcileRoots: []string{"/sessions"}, Reasons: watchReasonNativeRecovery,
	}, original.WatchRetryBatch())
}

func TestWatchReasonsCrossJSONTransport(t *testing.T) {
	batch := WatchBatch{FullSync: true, LostEvents: true, Reasons: watchReasonFallbackStarted | watchReasonStreamStart}
	data, err := json.Marshal(batch)
	require.NoError(t, err)
	assert.JSONEq(t, `{"full_sync":true,"lost_events":true,"reasons":144}`, string(data))
	var decoded WatchBatch
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, batch, decoded)
}

func TestWatchReasonLabelsAreStableAndBounded(t *testing.T) {
	for _, tc := range []struct {
		reasons WatchSyncReasons
		want    string
	}{
		{0, "none"},
		{watchReasonNativeRecovery | watchReasonCallbackRetry, "native_recovery,callback_retry"},
		{WatchSyncReasons(1 << 10), "kqueue_start_failed"},
		{WatchSyncReasons(1 << 14), "shallow_start_failed"},
		{WatchSyncReasons(1 << 31), "unknown_reason"},
		{^WatchSyncReasons(0), "pending_entries,pending_path_bytes,backend_event_loss,fallback_buffer_overflow,fallback_started,native_recovery,stream_create_failed,stream_start_failed,native_events_dropped,root_lifecycle,kqueue_start_failed,callback_retry,ambiguous_rename,caller_request,shallow_start_failed,unknown_reason"},
	} {
		assert.Equal(t, tc.want, tc.reasons.String())
	}
}
