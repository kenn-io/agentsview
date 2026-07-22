package main

import (
	"context"
	"log"
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
	logPrefix string
	debounce  time.Duration
	dirty     chan struct{}
	floor     <-chan time.Time
	after     func(time.Duration) <-chan time.Time
	push      func(ctx context.Context, reason pushReason) error
	// flushTimeout bounds the final shutdown-flush push. Zero means
	// no bound (used in tests that inject a fake pusher).
	flushTimeout time.Duration
}

// newPushLoop builds a production loop with a real debounce timer and
// floor ticker. The caller must Stop the returned ticker.
func newPushLoop(
	debounce, interval time.Duration,
	push func(context.Context, pushReason) error,
) (*pushLoop, *time.Ticker) {
	return newNamedPushLoop("pg watch", debounce, interval, push)
}

func newPushLoopWithLabel(
	label string,
	debounce, interval time.Duration,
	push func(context.Context, pushReason) error,
) (*pushLoop, *time.Ticker) {
	return newNamedPushLoop(label, debounce, interval, push)
}

func newNamedPushLoop(
	logPrefix string,
	debounce, interval time.Duration,
	push func(context.Context, pushReason) error,
) (*pushLoop, *time.Ticker) {
	ticker := time.NewTicker(interval)
	return &pushLoop{
		logPrefix:    logPrefix,
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
	if err := l.push(ctx, reason); err != nil {
		prefix := l.logPrefix
		if prefix == "" {
			prefix = "watch"
		}
		log.Printf("%s: push (%s) failed: %v", prefix, reason, err)
	}
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

	stopWatcher, unwatchedDirs := startFileWatcher(cfg.AppConfig, cfg.Engine,
		func(batch syncpkg.WatchBatch) {
			syncWatchBatch(ctx, cfg.Engine, batch)
			loop.NotifyDirty()
		},
	)
	defer stopWatcher()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"%s: %d root(s) not watched; relying on the %s floor for coverage",
			cfg.LogPrefix, len(unwatchedDirs), cfg.Interval,
		)
	}

	loop.Run(ctx)
}
