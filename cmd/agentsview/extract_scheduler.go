// ABOUTME: after-sync recall-extraction scheduler — debounces sync signals
// ABOUTME: into incremental passes and runs periodic full top-up passes.
package main

import (
	"context"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/recall/extract"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/sync"
)

// extractDebounceInterval is the quiet period the scheduler waits, after the
// last sync-completion signal, before running an extraction pass. Sessions
// only become eligible once the configured quiet period has elapsed since
// they ended, so this debounce merely batches scan work — it does not gate
// eligibility.
const extractDebounceInterval = 30 * time.Second

// extractPassManager is the subset of *extract.Manager the scheduler needs,
// letting tests substitute a fake that records TryPass calls.
type extractPassManager interface {
	TryPass(ctx context.Context, opts extract.PassOptions) (bool, extract.PassResult, error)
}

// extractScheduler mirrors the embed scheduler's shape for extraction:
// bursts of Notify calls collapse into one incremental pass after the
// debounce elapses, and a backstop ticker periodically runs a full pass
// that revisits done sessions so grown transcripts are topped up.
type extractScheduler struct {
	mgr      extractPassManager
	debounce time.Duration
	backstop time.Duration
	catchup  time.Duration
	// idle is the daemon's idle tracker; every pass runs under a work
	// lease so a detached daemon cannot idle out — cancelling the shared
	// context — under a long model-backed pass. Nil in foreground mode.
	idle *server.IdleTracker

	dirty chan struct{}
	stop  chan struct{}
	done  chan struct{}
}

// newExtractScheduler builds a scheduler over mgr. backstop <= 0 disables
// the periodic full pass; catchup then supplies the interval for periodic
// *incremental* passes instead. Without one, a session that ends and sees no
// further sync activity would never be scanned again once its quiet period
// elapses — sync-driven debounce passes fire long before it becomes
// eligible. catchup is ignored while the backstop is enabled, whose full
// passes are a superset.
func newExtractScheduler(
	mgr extractPassManager, debounce, backstop, catchup time.Duration,
	idle *server.IdleTracker,
) *extractScheduler {
	return &extractScheduler{
		mgr:      mgr,
		debounce: debounce,
		backstop: backstop,
		catchup:  catchup,
		idle:     idle,
		dirty:    make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Notify signals that sessions may have changed. It never blocks: dirty has
// capacity 1, so a burst of calls while Run is busy (or not yet started)
// coalesces into a single pending signal.
func (s *extractScheduler) Notify() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// Stop signals Run to exit and blocks until it has, so a caller that closes
// the database right after Stop can never race a pass still in flight.
func (s *extractScheduler) Stop() {
	close(s.stop)
	<-s.done
}

// Run debounces Notify signals into incremental TryPass calls and,
// independently, runs a full pass on every backstop tick. It returns when
// ctx is done or Stop is called.
func (s *extractScheduler) Run(ctx context.Context) {
	defer close(s.done)

	debounceTimer := time.NewTimer(s.debounce)
	stopTimer(debounceTimer)
	defer debounceTimer.Stop()

	var tickC <-chan time.Time
	tickFull := s.backstop > 0
	tickInterval := s.backstop
	if !tickFull {
		tickInterval = s.catchup
	}
	if tickInterval > 0 {
		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()
		tickC = ticker.C
	}

	// pendingFull remembers a backstop tick whose pass was dropped because
	// another pass was already running (typically a manual `recall extract
	// run`): without it, the digest top-up that tick carried would be
	// silently deferred until the next backstop tick instead of running on
	// the next debounced pass. Single-goroutine state, no locking needed.
	var pendingFull bool

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-s.dirty:
			resetTimer(debounceTimer, s.debounce)
		case <-debounceTimer.C:
			started, ok, err := s.tryPassWithLease(ctx,
				extract.PassOptions{Full: pendingFull})
			if !ok {
				// Draining: the daemon is shutting down, so no new pass
				// may start. Leave the loop parked on ctx/stop.
				continue
			}
			if err != nil {
				log.Printf("extract scheduler: pass failed: %v", err)
			}
			if !started {
				// A pass was already running elsewhere; re-arm rather
				// than drop this one. pendingFull, if set, stays set so
				// the retry still carries it.
				resetTimer(debounceTimer, s.debounce)
				continue
			}
			// Only clear a carried full pass once it both started and
			// succeeded; a started-but-failed pass never completed the
			// top-up it carried.
			if err == nil {
				pendingFull = false
			}
		case <-tickC:
			started, ok, err := s.tryPassWithLease(ctx,
				extract.PassOptions{Full: tickFull})
			if !ok {
				continue
			}
			if err != nil {
				log.Printf("extract scheduler: periodic pass failed: %v", err)
			}
			if tickFull {
				// A dropped incremental catchup tick needs no carry: the
				// next tick or debounced pass covers the same ground.
				pendingFull = !started || err != nil
			}
		}
	}
}

// tryPassWithLease runs one TryPass under an idle-tracker work lease, so a
// detached daemon never reaps itself — cancelling the shared context — while
// a model-backed pass is in flight. ok is false when the daemon is already
// draining, in which case no pass was attempted: work started after the
// idle decision would race the shutdown it triggered.
func (s *extractScheduler) tryPassWithLease(
	ctx context.Context, opts extract.PassOptions,
) (started, ok bool, err error) {
	release, ok := s.idle.BeginWork()
	if !ok {
		return false, false, nil
	}
	defer release()
	started, _, err = s.mgr.TryPass(ctx, opts)
	return started, true, err
}

// extractTeeEmitter fans a sync completion out to the wrapped emitter and
// the extraction scheduler. Notify never blocks, so this cannot slow the
// sync pipeline.
type extractTeeEmitter struct {
	primary   sync.Emitter
	scheduler *extractScheduler
}

func (t extractTeeEmitter) Emit(scope string) {
	t.primary.Emit(scope)
	t.scheduler.Notify()
}
