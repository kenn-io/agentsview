package extract

import (
	"context"
	"sync"
	"time"
)

// Reconciler retracts the generated corpus of sessions that lost extraction
// eligibility, independently of the model-backed extraction loop. The daemon
// runs it even when [recall.extract] is disabled: a generation activated
// while extraction was enabled keeps serving its entries, and those must
// stop being served once their source session is trashed, flagged
// automated, or gains secret findings. Gating retraction on extraction being
// enabled would leave that privacy obligation unmet.
//
// It implements the same TryPass shape the scheduler drives Manager with, so
// the existing debounce/lease/ticker machinery reconciles on startup, on
// session-mutation notifications, and periodically.
type Reconciler struct {
	db reconcileStore

	mu        sync.Mutex
	watermark time.Time
}

// reconcileStore is the subset of *db.DB the Reconciler needs.
type reconcileStore interface {
	ReconcileIneligibleExtractSessions(
		ctx context.Context, changedSince time.Time,
	) (int, int, error)
}

// NewReconciler builds a Reconciler over store.
func NewReconciler(store reconcileStore) *Reconciler {
	return &Reconciler{db: store}
}

// TryPass runs one reconciliation pass unless one is already in flight,
// reporting whether it ran. The watermark ratchets forward to the pass start
// so later passes only re-walk sessions written since — every ineligibility
// write records a local write, so nothing is missed.
func (r *Reconciler) TryPass(
	ctx context.Context, _ PassOptions,
) (bool, PassResult, error) {
	if !r.mu.TryLock() {
		return false, PassResult{}, nil
	}
	defer r.mu.Unlock()
	passStart := time.Now()
	if _, _, err := r.db.ReconcileIneligibleExtractSessions(
		ctx, r.watermark,
	); err != nil {
		return true, PassResult{}, err
	}
	if passStart.After(r.watermark) {
		r.watermark = passStart
	}
	return true, PassResult{}, nil
}
