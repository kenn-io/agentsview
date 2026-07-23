package main

import (
	"context"
	"log"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
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
	push      func(ctx context.Context, reason pushReason) error
	label     string
	pendingMu sync.Mutex
	pending   bool
	waiters   []chan error
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
	push func(context.Context, pushReason) error,
) (*pushLoop, *time.Ticker) {
	return newNamedPushLoop(label, debounce, interval, push)
}

func newNamedPushLoop(
	label string,
	debounce, interval time.Duration,
	push func(context.Context, pushReason) error,
) (*pushLoop, *time.Ticker) {
	ticker := time.NewTicker(interval)
	return &pushLoop{
		label:        label,
		debounce:     debounce,
		dirty:        make(chan struct{}, 1),
		floor:        ticker.C,
		after:        time.After,
		push:         push,
		flushTimeout: defaultFlushTimeout,
	}, ticker
}

// NotifyDirty signals that local data changed. Non-blocking: a burst
// collapses into a single pending push.
func (l *pushLoop) NotifyDirty() {
	l.pendingMu.Lock()
	l.pending = true
	l.pendingMu.Unlock()
	l.signalDirty()
}

// NotifyDirtyWithAck marks the loop dirty and returns immediately. The result
// channel completes only after a push covering this generation succeeds;
// failed pushes retain both the dirty marker and every waiter for a retry.
func (l *pushLoop) NotifyDirtyWithAck() <-chan error {
	waiter := make(chan error, 1)
	l.pendingMu.Lock()
	l.pending = true
	l.waiters = append(l.waiters, waiter)
	l.pendingMu.Unlock()
	l.signalDirty()
	return waiter
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
			l.doPush(flushCtx, reasonShutdown)
			return
		case <-l.dirty:
			if !armed {
				armed = true
				fire = l.after(l.debounce)
			}
		case <-fire:
			armed = false
			fire = nil
			l.doPush(ctx, reasonChange)
		case <-l.floor:
			// A floor tick supersedes any pending debounce.
			armed = false
			fire = nil
			l.doPush(ctx, reasonInterval)
		}
	}
}

func (l *pushLoop) doPush(ctx context.Context, reason pushReason) {
	hadPending, waiters := l.claimPending()
	if err := l.push(ctx, reason); err != nil {
		log.Printf("%s: push (%s) failed: %v", l.label, reason, err)
		if hadPending {
			l.restorePending(waiters)
		}
		return
	}
	for _, waiter := range waiters {
		waiter <- nil
		close(waiter)
	}
}

func (l *pushLoop) claimPending() (bool, []chan error) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	hadPending := l.pending
	waiters := l.waiters
	l.pending = false
	l.waiters = nil
	return hadPending, waiters
}

func (l *pushLoop) restorePending(waiters []chan error) {
	l.pendingMu.Lock()
	l.pending = true
	if len(waiters) > 0 {
		l.waiters = append(waiters, l.waiters...)
	}
	l.pendingMu.Unlock()
	l.signalDirty()
}

type watchedSinkConfig struct {
	AppConfig config.Config
	Engine    *syncpkg.Engine
	Debounce  time.Duration
	Interval  time.Duration
	LogPrefix string
	Push      func(context.Context, pushReason) error
}

func runWatchedSink(ctx context.Context, cfg watchedSinkConfig) {
	loop, ticker := newNamedPushLoop(
		cfg.LogPrefix, cfg.Debounce, cfg.Interval, cfg.Push,
	)
	defer ticker.Stop()

	stopWatcher, openDispatch, unwatchedDirs, _ := startFileWatcher(
		cfg.AppConfig, cfg.Engine,
		func(callbackCtx context.Context, batch syncpkg.WatchBatch) error {
			scope := func() watchRecoveryScope {
				return probeWatchRecoveryScope(cfg.AppConfig)
			}
			if err := syncWatchBatch(callbackCtx, cfg.Engine, batch, scope); err != nil {
				return err
			}
			loop.NotifyDirty()
			return nil
		},
		syncpkg.WatcherOptions{OnCoverageDegraded: loop.NotifyCoverageDegraded},
	)
	defer stopWatcher()
	openDispatch()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"%s: %d root(s) not watched; relying on the %s floor for coverage",
			cfg.LogPrefix, len(unwatchedDirs), cfg.Interval,
		)
	}

	loop.Run(ctx)
}
