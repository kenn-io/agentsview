package sync

import "strings"

// WatchSyncReasons records diagnostic causes of watcher syncs and retries.
type WatchSyncReasons uint32

// Append new flags so existing serialized values keep their meaning.
const (
	watchReasonPendingEntries WatchSyncReasons = 1 << iota
	watchReasonPendingBytes
	watchReasonBackendEventLoss
	watchReasonFallbackBuffer
	watchReasonFallbackStarted
	watchReasonNativeRecovery
	watchReasonStreamCreate
	watchReasonStreamStart
	watchReasonNativeDrop
	watchReasonRootLifecycle
	watchReasonKqueueStart
	watchReasonCallbackRetry
	watchReasonAmbiguousRename
	watchReasonCallerRequest
	watchReasonShallowStart
)

var watchSyncReasonLabels = [...]struct {
	reason WatchSyncReasons
	label  string
}{
	{watchReasonPendingEntries, "pending_entries"},
	{watchReasonPendingBytes, "pending_path_bytes"},
	{watchReasonBackendEventLoss, "backend_event_loss"},
	{watchReasonFallbackBuffer, "fallback_buffer_overflow"},
	{watchReasonFallbackStarted, "fallback_started"},
	{watchReasonNativeRecovery, "native_recovery"},
	{watchReasonStreamCreate, "stream_create_failed"},
	{watchReasonStreamStart, "stream_start_failed"},
	{watchReasonNativeDrop, "native_events_dropped"},
	{watchReasonRootLifecycle, "root_lifecycle"},
	{watchReasonKqueueStart, "kqueue_start_failed"},
	{watchReasonCallbackRetry, "callback_retry"},
	{watchReasonAmbiguousRename, "ambiguous_rename"},
	{watchReasonCallerRequest, "caller_request"},
	{watchReasonShallowStart, "shallow_start_failed"},
}

// String returns stable labels without paths or other source data.
func (reasons WatchSyncReasons) String() string {
	var labels []string
	var known WatchSyncReasons
	for _, entry := range watchSyncReasonLabels {
		known |= entry.reason
		if reasons&entry.reason != 0 {
			labels = append(labels, entry.label)
		}
	}
	if reasons&^known != 0 {
		labels = append(labels, "unknown_reason")
	}
	if len(labels) == 0 {
		return "none"
	}
	return strings.Join(labels, ",")
}

func (batch WatchBatch) reconciliationReasons() WatchSyncReasons {
	if batch.Reasons == 0 && batch.FullSync {
		return watchReasonCallerRequest
	}
	return batch.Reasons
}

func (p *pendingWatchBatch) takeReasons() WatchSyncReasons {
	reasons := p.reasons
	p.reasons = 0
	return reasons
}
