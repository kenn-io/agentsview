// ABOUTME: Per-session debouncer for signal/secret recomputation on
// ABOUTME: the incremental write path (leading-edge run + quiet flush).
package sync

import (
	"slices"
	"sync"
	"time"
)

// Debounce parameters for signal recomputation triggered by
// incremental writes. Recomputing signals costs O(session history)
// (full message reload plus regex secret scan), so during streaming
// bursts a session recomputes at most once per
// signalRecomputeInterval, with a trailing flush once the session
// has been quiet for signalRecomputeQuiet.
const (
	signalRecomputeInterval = 10 * time.Second
	signalRecomputeQuiet    = 2 * time.Second
)

// signalScheduler coalesces per-session recompute requests. The
// first request for a quiet session runs inline immediately
// (leading edge), further requests within the interval are
// deferred, and a one-shot timer flushes deferred sessions once
// they go quiet or their interval elapses. No timer is armed while
// nothing is dirty, so an idle scheduler costs nothing.
type signalScheduler struct {
	interval time.Duration
	quiet    time.Duration
	// run recomputes inline from markDirty, whose callers already
	// hold the engine's sync lock. deferredRun recomputes from the
	// flush timer and explicit flushes, which run outside any sync
	// operation and must serialize with sync writes themselves.
	run         func(sessionID string)
	deferredRun func(sessionID string)

	// now and afterFunc are injectable for deterministic tests.
	now       func() time.Time
	afterFunc func(d time.Duration, f func())

	mu         sync.Mutex
	last       map[string]time.Time // sessionID -> last recompute
	dirty      map[string]time.Time // sessionID -> last deferred mark
	timerArmed bool
	stopped    bool
}

func newSignalScheduler(
	interval, quiet time.Duration,
	run, deferredRun func(sessionID string),
) *signalScheduler {
	return &signalScheduler{
		interval:    interval,
		quiet:       quiet,
		run:         run,
		deferredRun: deferredRun,
		now:         time.Now,
		afterFunc:   func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		last:        make(map[string]time.Time),
		dirty:       make(map[string]time.Time),
	}
}

// markDirty requests a signal recompute for the session. It runs
// inline when the session hasn't recomputed within the interval
// (or the scheduler is stopped); otherwise it defers the recompute
// and arms the flush timer.
func (s *signalScheduler) markDirty(sessionID string) {
	s.mu.Lock()
	now := s.now()
	if s.stopped || now.Sub(s.last[sessionID]) >= s.interval {
		s.last[sessionID] = now
		delete(s.dirty, sessionID)
		s.mu.Unlock()
		s.run(sessionID)
		return
	}
	s.dirty[sessionID] = now
	s.armLocked()
	s.mu.Unlock()
}

// tick flushes deferred sessions whose interval has elapsed or
// that have been quiet long enough.
func (s *signalScheduler) tick() {
	s.runAll(s.takeDue(false))
}

// flushAll immediately recomputes every deferred session.
func (s *signalScheduler) flushAll() {
	s.runAll(s.takeDue(true))
}

// stop flushes pending recomputes and puts the scheduler in
// pass-through mode: later marks recompute inline and no timers
// are armed. Used at engine shutdown; safe to call repeatedly.
func (s *signalScheduler) stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.flushAll()
}

func (s *signalScheduler) runAll(sessionIDs []string) {
	for _, id := range sessionIDs {
		s.deferredRun(id)
	}
}

// takeDue removes and returns the sessions ready to recompute,
// stamping their recompute time. With all set, every dirty session
// is due.
func (s *signalScheduler) takeDue(all bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var due []string
	for id, markedAt := range s.dirty {
		if all ||
			now.Sub(s.last[id]) >= s.interval ||
			now.Sub(markedAt) >= s.quiet {
			due = append(due, id)
			s.last[id] = now
			delete(s.dirty, id)
		}
	}
	// Drop stale recompute stamps so the map doesn't accumulate an
	// entry for every session ever synced.
	for id, at := range s.last {
		if _, pending := s.dirty[id]; !pending &&
			now.Sub(at) >= 10*s.interval {
			delete(s.last, id)
		}
	}
	slices.Sort(due)
	return due
}

// armLocked schedules the one-shot flush timer if it isn't already
// pending. Caller must hold s.mu.
func (s *signalScheduler) armLocked() {
	if s.timerArmed || s.stopped {
		return
	}
	s.timerArmed = true
	s.afterFunc(s.quiet, s.onTimer)
}

// onTimer is the flush-timer callback: flush what is due, then
// re-arm while sessions remain dirty.
func (s *signalScheduler) onTimer() {
	s.mu.Lock()
	s.timerArmed = false
	s.mu.Unlock()

	s.tick()

	s.mu.Lock()
	if len(s.dirty) > 0 {
		s.armLocked()
	}
	s.mu.Unlock()
}
