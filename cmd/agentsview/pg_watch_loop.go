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
	debounce        time.Duration
	dirty           chan struct{}
	floor           <-chan time.Time
	after           func(time.Duration) <-chan time.Time
	push            func(ctx context.Context, reason pushReason, batch *syncpkg.WatchBatch) error
	label           string
	pendingMu       sync.Mutex
	pendingTasks    []queuedPushTask
	promotionCounts map[syncpkg.WatchBatchPromotionReason]int
	// flushTimeout bounds the final shutdown-flush push. Zero means
	// no bound (used in tests that inject a fake pusher).
	flushTimeout time.Duration
}

// queuedPushTask is one unit of work owned by the push loop. Producers enqueue
// scope; only Run removes and executes tasks. An unscoped task covers every
// earlier pending task, while scoped work queued after it remains separate.
type queuedPushTask struct {
	reason   pushReason
	unscoped bool
	batch    *syncpkg.WatchBatchAccumulator
	waiters  []chan error
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
	l.enqueueUnscoped(reasonChange, nil, true)
}

// NotifyBatch retains one bounded watcher batch for the next push. Adjacent
// scoped notifications merge; work arriving after an unscoped task stays next.
func (l *pushLoop) NotifyBatch(batch syncpkg.WatchBatch) {
	l.enqueueBatch(batch, nil)
}

// NotifyDirtyWithAck marks the loop dirty and returns immediately. The result
// channel completes only after a push covering this generation succeeds;
// failed pushes retain both the dirty marker and every waiter for a retry.
func (l *pushLoop) NotifyDirtyWithAck() <-chan error {
	waiter := make(chan error, 1)
	l.enqueueUnscoped(reasonChange, waiter, true)
	return waiter
}

// NotifyBatchWithAck retains a bounded watcher batch and completes its waiter
// only after the queued task that includes or supersedes that scope succeeds.
func (l *pushLoop) NotifyBatchWithAck(batch syncpkg.WatchBatch) <-chan error {
	waiter := make(chan error, 1)
	l.enqueueBatch(batch, waiter)
	return waiter
}

func (l *pushLoop) enqueueBatch(
	batch syncpkg.WatchBatch,
	waiter chan error,
) {
	l.pendingMu.Lock()
	last := len(l.pendingTasks) - 1
	if last < 0 || l.pendingTasks[last].unscoped {
		l.pendingTasks = append(l.pendingTasks, queuedPushTask{
			reason: reasonChange,
			batch:  l.newBatchAccumulatorLocked(),
		})
		last++
	}
	l.pendingTasks[last].batch.Add(batch)
	if waiter != nil {
		l.pendingTasks[last].waiters = append(
			l.pendingTasks[last].waiters, waiter,
		)
	}
	l.pendingMu.Unlock()
	l.signalDirty()
}

func (l *pushLoop) enqueueUnscoped(
	reason pushReason,
	waiter chan error,
	signal bool,
) {
	l.pendingMu.Lock()
	task := queuedPushTask{reason: reason, unscoped: true}
	for i := range l.pendingTasks {
		task.waiters = append(task.waiters, l.pendingTasks[i].waiters...)
	}
	if waiter != nil {
		task.waiters = append(task.waiters, waiter)
	}
	l.pendingTasks = []queuedPushTask{task}
	l.pendingMu.Unlock()
	if signal {
		l.signalDirty()
	}
}

func (l *pushLoop) newBatchAccumulatorLocked() *syncpkg.WatchBatchAccumulator {
	return syncpkg.NewWatchBatchAccumulator(
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
			l.enqueueShutdownTask()
			l.runNextTask(flushCtx, true)
			return
		case <-l.dirty:
			if !armed {
				armed = true
				fire = l.after(l.debounce)
			}
		case <-fire:
			armed = false
			fire = nil
			l.runNextTask(ctx, false)
		case <-l.floor:
			// The timer produces an unscoped task. It covers older queued
			// watcher work; watcher tasks arriving during it stay queued.
			armed = false
			fire = nil
			l.enqueueUnscoped(reasonInterval, nil, false)
			l.runNextTask(ctx, false)
		}
	}
}

type pushClaim struct {
	reason   pushReason
	unscoped bool
	batch    *syncpkg.WatchBatch
	waiters  []chan error
}

func (l *pushLoop) runNextTask(ctx context.Context, final bool) {
	claim, ok := l.claimPending()
	if !ok {
		return
	}
	if err := l.push(ctx, claim.reason, claim.batch); err != nil {
		log.Printf("%s: push (%s) failed: %v", l.label, claim.reason, err)
		if final {
			completePushWaiters(claim.waiters, err)
		} else {
			l.restorePending(claim)
		}
		return
	}
	completePushWaiters(claim.waiters, nil)
	l.signalPending()
}

func (l *pushLoop) claimPending() (pushClaim, bool) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if len(l.pendingTasks) == 0 {
		return pushClaim{}, false
	}
	task := l.pendingTasks[0]
	l.pendingTasks = l.pendingTasks[1:]
	claim := pushClaim{
		reason:   task.reason,
		unscoped: task.unscoped,
		waiters:  task.waiters,
	}
	if !claim.unscoped && task.batch != nil {
		if batch, ok := task.batch.Take(); ok {
			claim.batch = &batch
		}
	}
	return claim, true
}

func (l *pushLoop) restorePending(claim pushClaim) {
	l.pendingMu.Lock()
	if !claim.unscoped && len(l.pendingTasks) > 0 && l.pendingTasks[0].unscoped {
		l.pendingTasks[0].waiters = append(
			claim.waiters, l.pendingTasks[0].waiters...,
		)
		l.pendingMu.Unlock()
		l.signalDirty()
		return
	}
	task := queuedPushTask{
		reason: claim.reason, unscoped: claim.unscoped,
		waiters: claim.waiters,
	}
	if claim.batch != nil {
		task.batch = l.newBatchAccumulatorLocked()
		task.batch.Add(*claim.batch)
	}
	if !task.unscoped && len(l.pendingTasks) > 0 && !l.pendingTasks[0].unscoped {
		next := l.pendingTasks[0]
		if batch, ok := next.batch.Take(); ok {
			task.batch.Add(batch)
		}
		task.waiters = append(task.waiters, next.waiters...)
		l.pendingTasks = l.pendingTasks[1:]
	}
	l.pendingTasks = append([]queuedPushTask{task}, l.pendingTasks...)
	l.pendingMu.Unlock()
	l.signalDirty()
}

func (l *pushLoop) enqueueShutdownTask() {
	l.pendingMu.Lock()
	task := queuedPushTask{reason: reasonShutdown}
	if len(l.pendingTasks) == 0 {
		task.unscoped = true
	}
	for i := range l.pendingTasks {
		pending := &l.pendingTasks[i]
		task.waiters = append(task.waiters, pending.waiters...)
		if pending.unscoped {
			task.unscoped = true
			task.batch = nil
			continue
		}
		if task.unscoped || pending.batch == nil {
			continue
		}
		if task.batch == nil {
			task.batch = l.newBatchAccumulatorLocked()
		}
		if batch, ok := pending.batch.Take(); ok {
			task.batch.Add(batch)
		}
	}
	l.pendingTasks = []queuedPushTask{task}
	l.pendingMu.Unlock()
}

func (l *pushLoop) signalPending() {
	l.pendingMu.Lock()
	pending := len(l.pendingTasks) > 0
	l.pendingMu.Unlock()
	if pending {
		l.signalDirty()
	}
}

func completePushWaiters(waiters []chan error, err error) {
	for _, waiter := range waiters {
		waiter <- err
		close(waiter)
	}
}
