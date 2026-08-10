package sync

// WatchBatchPromotionReason identifies which public accumulator bound forced
// fine-grained watcher work to become an authoritative full sync.
type WatchBatchPromotionReason string

const (
	WatchBatchPromotionEntryLimit WatchBatchPromotionReason = "entry_limit"
	WatchBatchPromotionByteLimit  WatchBatchPromotionReason = "byte_limit"
)

// WatchRecoveryScope is the transport-safe availability snapshot used when a
// full or rename batch needs authoritative reconciliation.
type WatchRecoveryScope struct {
	AvailableRoots []string `json:"available_roots,omitempty"`
	DeferredRoots  []string `json:"deferred_roots,omitempty"`
}

// WatchBatchAccumulator coalesces public watcher work under the same entry and
// byte limits as the watcher itself. Backend lifecycle tokens never enter it.
type WatchBatchAccumulator struct {
	pending *pendingWatchBatch
}

func NewWatchBatchAccumulator(
	onPromote func(WatchBatchPromotionReason),
) *WatchBatchAccumulator {
	pending := newPendingWatchBatch(
		defaultWatchBatchMaxEntries,
		defaultWatchBatchMaxPathBytes,
	)
	pending.onOverflow = onPromote
	return &WatchBatchAccumulator{pending: pending}
}

func (a *WatchBatchAccumulator) Add(batch WatchBatch) {
	if a.pending.fullSync {
		a.pending.lostEvents = a.pending.lostEvents || batch.LostEvents
		return
	}
	if batch.FullSync {
		a.pending.makeFullSync(batch.LostEvents)
		return
	}
	for _, path := range batch.Paths {
		a.pending.Add(path)
		if a.pending.fullSync {
			return
		}
	}
	for _, rename := range batch.Renames {
		a.pending.AddRename(rename)
		if a.pending.fullSync {
			return
		}
	}
	for _, root := range batch.ReconcileRoots {
		a.pending.AddReconcileRoot(root)
		if a.pending.fullSync {
			return
		}
	}
	a.pending.lostEvents = a.pending.lostEvents || batch.LostEvents
}

func (a *WatchBatchAccumulator) Take() (WatchBatch, bool) {
	return a.pending.Take()
}

func (a *WatchBatchAccumulator) Empty() bool {
	return a.pending.Empty()
}
