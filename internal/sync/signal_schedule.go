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
	// hold the engine's sync lock. exclusive wraps deferred flushes
	// (timer ticks, flushAll), which run outside any sync operation:
	// it acquires that lock around the whole claim-and-recompute
	// pass, so sessions are never claimed out of the dirty map by a
	// goroutine that then blocks — a concurrent locked flush would
	// see an empty map and push stale rows.
	run       func(sessionID string)
	exclusive func(flush func())

	// now and afterFunc are injectable for deterministic tests.
	// afterFunc returns a cancel function reporting whether the
	// callback was stopped before it started running.
	now       func() time.Time
	afterFunc func(d time.Duration, f func()) (cancel func() bool)

	// inflight counts armed flush timers whose callbacks have not
	// finished, so stop can wait for a recompute already running
	// on the timer goroutine before its owner closes the DB.
	inflight sync.WaitGroup

	mu          sync.Mutex
	last        map[string]time.Time // sessionID -> last recompute
	dirty       map[string]time.Time // sessionID -> last deferred mark
	timerArmed  bool
	timerCancel func() bool
	stopped     bool
}

func newSignalScheduler(
	interval, quiet time.Duration,
	run func(sessionID string),
	exclusive func(flush func()),
) *signalScheduler {
	return &signalScheduler{
		interval:  interval,
		quiet:     quiet,
		run:       run,
		exclusive: exclusive,
		now:       time.Now,
		afterFunc: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
		last:  make(map[string]time.Time),
		dirty: make(map[string]time.Time),
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
// that have been quiet long enough. Callers must not hold the
// engine's sync lock: the pass runs inside exclusive, which takes
// it before claiming anything.
func (s *signalScheduler) tick() {
	s.exclusive(func() { s.flushDue(false) })
}

// flushAll immediately recomputes every deferred session. Callers
// must not hold the engine's sync lock (see tick).
func (s *signalScheduler) flushAll() {
	s.exclusive(func() { s.flushDue(true) })
}

// flushAllInline immediately recomputes every deferred session
// without entering exclusive. For callers already holding the
// engine's sync lock (the context markDirty's inline runs execute
// under), where exclusive would deadlock.
func (s *signalScheduler) flushAllInline() {
	s.flushDue(true)
}

func (s *signalScheduler) flushDue(all bool) {
	for _, id := range s.takeDue(all) {
		s.run(id)
	}
}

// stop cancels the pending flush timer, waits for any timer
// callback already running (so no recompute is still using the DB
// when stop returns), flushes remaining deferred recomputes, and
// puts the scheduler in pass-through mode: later marks recompute
// inline and no timers are armed. Used at engine shutdown; safe to
// call repeatedly.
func (s *signalScheduler) stop() {
	s.mu.Lock()
	s.stopped = true
	cancel := s.timerCancel
	armed := s.timerArmed
	s.timerArmed = false
	s.timerCancel = nil
	s.mu.Unlock()
	// A successful cancel means the callback will never run, so its
	// inflight slot is released here; otherwise the callback is
	// already running (or about to) and releases it itself.
	if armed && cancel != nil && cancel() {
		s.inflight.Done()
	}
	s.inflight.Wait()
	s.flushAll()
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
	s.inflight.Add(1)
	s.timerCancel = s.afterFunc(s.quiet, s.onTimer)
}

// onTimer is the flush-timer callback: flush what is due, then
// re-arm while sessions remain dirty. The deferred Done runs after
// any re-arm's Add, so the inflight count never dips to zero while
// a follow-up timer is pending.
func (s *signalScheduler) onTimer() {
	defer s.inflight.Done()
	s.mu.Lock()
	s.timerArmed = false
	s.timerCancel = nil
	s.mu.Unlock()

	s.tick()

	s.mu.Lock()
	if len(s.dirty) > 0 {
		s.armLocked()
	}
	s.mu.Unlock()
}
