package main

import (
	"context"
	"log"
	"sync"
	"time"

	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// pushReason labels why a push was triggered, for logging.
type pushReason string

const (
	reasonStartup  pushReason = "startup"
	reasonChange   pushReason = "change"
	reasonInterval pushReason = "interval"
	reasonShutdown pushReason = "shutdown"
)

// defaultFlushTimeout bounds the best-effort push performed when the
// loop shuts down, so a stalled PostgreSQL connection cannot block
// process exit indefinitely.
const defaultFlushTimeout = 30 * time.Second

// pushLoop coalesces file-change notifications and a periodic floor
// tick into serialized pushes. A single goroutine (Run) performs all
// pushes, so a push is never concurrent with another push.
//
// The after/floor fields are injectable so the loop is deterministic
// under test. In production, after is time.After and floor is a
// time.Ticker channel.
type pushLoop struct {
	debounce  time.Duration
	dirty     chan struct{}
	floor     <-chan time.Time
	after     func(time.Duration) <-chan time.Time
	push      func(ctx context.Context, reason pushReason, batch *syncpkg.WatchBatch) error
	label     string
	pendingMu sync.Mutex
	// pendingUnscoped supersedes pendingBatch. Startup, interval, coverage,
	// and manual dirtiness intentionally keep the historical full SyncAll path.
	pendingUnscoped bool
	pendingBatch    *syncpkg.WatchBatchAccumulator
	waiters         []chan error
	promotionCounts map[syncpkg.WatchBatchPromotionReason]int
	// flushTimeout bounds the final shutdown-flush push. Zero means
	// no bound (used in tests that inject a fake pusher).
	flushTimeout time.Duration
}

// NotifyCoverageDegraded logs that the watcher lost coverage of roots and
// marks the loop dirty so the interval floor re-pushes the affected data.
func (l *pushLoop) NotifyCoverageDegraded(roots []string) error {
	log.Printf(
		"%s: watcher coverage degraded root_count=%d", l.label, len(roots),
	)
	l.NotifyDirty()
	return nil
}

func newPushLoopWithLabel(
	label string,
	debounce, interval time.Duration,
	push func(context.Context, pushReason, *syncpkg.WatchBatch) error,
) (*pushLoop, *time.Ticker) {
	ticker := time.NewTicker(interval)
	return &pushLoop{
		debounce:        debounce,
		dirty:           make(chan struct{}, 1),
		floor:           ticker.C,
		after:           time.After,
		push:            push,
		label:           label,
		flushTimeout:    defaultFlushTimeout,
		promotionCounts: make(map[syncpkg.WatchBatchPromotionReason]int),
	}, ticker
}

// NotifyDirty signals that local data changed. Non-blocking: a burst
// collapses into a single pending push.
func (l *pushLoop) NotifyDirty() {
	l.pendingMu.Lock()
	l.pendingUnscoped = true
	l.pendingBatch = nil
	l.pendingMu.Unlock()
	l.signalDirty()
}

// NotifyBatch retains one bounded watcher batch for the next push. A pending
// unscoped notification already covers it and remains authoritative.
func (l *pushLoop) NotifyBatch(batch syncpkg.WatchBatch) {
	l.pendingMu.Lock()
	if !l.pendingUnscoped {
		l.batchAccumulatorLocked().Add(batch)
	}
	l.pendingMu.Unlock()
	l.signalDirty()
}

// NotifyDirtyWithAck marks the loop dirty and returns immediately. The result
// channel completes only after a push covering this generation succeeds;
// failed pushes retain both the dirty marker and every waiter for a retry.
func (l *pushLoop) NotifyDirtyWithAck() <-chan error {
	waiter := make(chan error, 1)
	l.pendingMu.Lock()
	l.pendingUnscoped = true
	l.pendingBatch = nil
	l.waiters = append(l.waiters, waiter)
	l.pendingMu.Unlock()
	l.signalDirty()
	return waiter
}

// NotifyBatchWithAck retains a bounded watcher batch and completes its waiter
// only after a push that includes or supersedes that scope succeeds.
func (l *pushLoop) NotifyBatchWithAck(batch syncpkg.WatchBatch) <-chan error {
	waiter := make(chan error, 1)
	l.pendingMu.Lock()
	if !l.pendingUnscoped {
		l.batchAccumulatorLocked().Add(batch)
	}
	l.waiters = append(l.waiters, waiter)
	l.pendingMu.Unlock()
	l.signalDirty()
	return waiter
}

func (l *pushLoop) batchAccumulatorLocked() *syncpkg.WatchBatchAccumulator {
	if l.pendingBatch == nil {
		l.pendingBatch = syncpkg.NewWatchBatchAccumulator(
			func(reason syncpkg.WatchBatchPromotionReason) {
				if l.promotionCounts == nil {
					l.promotionCounts = make(map[syncpkg.WatchBatchPromotionReason]int)
				}
				l.promotionCounts[reason]++
				log.Printf(
					"%s: watcher batch promoted reason=%s promotion_count=%d",
					l.label, reason, l.promotionCounts[reason],
				)
			},
		)
	}
	return l.pendingBatch
}

func (l *pushLoop) signalDirty() {
	select {
	case l.dirty <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is cancelled, then performs a final flush push.
func (l *pushLoop) Run(ctx context.Context) {
	var armed bool
	var fire <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			// Final best-effort flush with a fresh context so the
			// push is not immediately cancelled.
			flushCtx := context.Background()
			if l.flushTimeout > 0 {
				var cancel context.CancelFunc
				flushCtx, cancel = context.WithTimeout(flushCtx, l.flushTimeout)
				defer cancel()
			}
			l.doPush(flushCtx, reasonShutdown, true)
			return
		case <-l.dirty:
			if !armed {
				armed = true
				fire = l.after(l.debounce)
			}
		case <-fire:
			armed = false
			fire = nil
			l.doPush(ctx, reasonChange, false)
		case <-l.floor:
			// A floor tick supersedes any pending debounce.
			armed = false
			fire = nil
			l.doPush(ctx, reasonInterval, false)
		}
	}
}

type pushClaim struct {
	hadPending bool
	unscoped   bool
	batch      *syncpkg.WatchBatch
	waiters    []chan error
}

func (l *pushLoop) doPush(ctx context.Context, reason pushReason, final bool) {
	claim := l.claimPending()
	if err := l.push(ctx, reason, claim.batch); err != nil {
		log.Printf("%s: push (%s) failed: %v", l.label, reason, err)
		if claim.hadPending {
			if final {
				completePushWaiters(claim.waiters, err)
			} else {
				l.restorePending(claim)
			}
		}
		return
	}
	completePushWaiters(claim.waiters, nil)
}

func (l *pushLoop) claimPending() pushClaim {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	claim := pushClaim{
		hadPending: l.pendingUnscoped ||
			(l.pendingBatch != nil && !l.pendingBatch.Empty()),
		unscoped: l.pendingUnscoped,
		waiters:  l.waiters,
	}
	if !claim.unscoped && l.pendingBatch != nil {
		if batch, ok := l.pendingBatch.Take(); ok {
			claim.batch = &batch
		}
	}
	l.pendingUnscoped = false
	l.pendingBatch = nil
	l.waiters = nil
	return claim
}

func (l *pushLoop) restorePending(claim pushClaim) {
	l.pendingMu.Lock()
	if claim.unscoped {
		l.pendingUnscoped = true
		l.pendingBatch = nil
	} else if claim.batch != nil && !l.pendingUnscoped {
		l.batchAccumulatorLocked().Add(*claim.batch)
	}
	if len(claim.waiters) > 0 {
		l.waiters = append(claim.waiters, l.waiters...)
	}
	l.pendingMu.Unlock()
	l.signalDirty()
}

func completePushWaiters(waiters []chan error, err error) {
	for _, waiter := range waiters {
		waiter <- err
		close(waiter)
	}
}
