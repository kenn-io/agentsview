package sync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type harnessTimer struct {
	f        func()
	fired    bool
	canceled bool
}

type schedulerHarness struct {
	mu          sync.Mutex
	sched       *signalScheduler
	clock       time.Time
	runs        []string
	deferred    []string // runs that happened inside exclusive
	inExclusive bool
	armed       []*harnessTimer
}

func newSchedulerHarness(interval, quiet time.Duration) *schedulerHarness {
	h := &schedulerHarness{clock: time.Unix(1_700_000_000, 0)}
	record := func(id string) {
		h.mu.Lock()
		h.runs = append(h.runs, id)
		if h.inExclusive {
			h.deferred = append(h.deferred, id)
		}
		h.mu.Unlock()
	}
	h.sched = newSignalScheduler(interval, quiet, record,
		func(flush func()) {
			h.mu.Lock()
			h.inExclusive = true
			h.mu.Unlock()
			flush()
			h.mu.Lock()
			h.inExclusive = false
			h.mu.Unlock()
		})
	h.sched.now = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.clock
	}
	h.sched.afterFunc = func(_ time.Duration, f func()) func() bool {
		h.mu.Lock()
		ht := &harnessTimer{f: f}
		h.armed = append(h.armed, ht)
		h.mu.Unlock()
		return func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			if ht.fired || ht.canceled {
				return false
			}
			ht.canceled = true
			return true
		}
	}
	return h
}

func (h *schedulerHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.clock = h.clock.Add(d)
	h.mu.Unlock()
}

func (h *schedulerHarness) runsSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.runs...)
}

func (h *schedulerHarness) deferredSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.deferred...)
}

// armedCount returns the number of pending (not fired, not
// canceled) timers.
func (h *schedulerHarness) armedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, ht := range h.armed {
		if !ht.fired && !ht.canceled {
			n++
		}
	}
	return n
}

// fireTimer invokes the oldest pending timer callback, simulating
// its expiry regardless of the fake clock.
func (h *schedulerHarness) fireTimer(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	var pending *harnessTimer
	for _, ht := range h.armed {
		if !ht.fired && !ht.canceled {
			pending = ht
			break
		}
	}
	require.NotNil(t, pending, "no timer armed")
	pending.fired = true
	h.mu.Unlock()
	pending.f()
}

func TestSignalSchedulerFirstMarkRunsInline(t *testing.T) {
	h := newSchedulerHarness(10*time.Second, 2*time.Second)

	h.sched.markDirty("s1")

	assert.Equal(t, []string{"s1"}, h.runsSnapshot(),
		"first mark after quiet should recompute immediately")
	assert.Zero(t, h.armedCount(),
		"inline run should not arm a flush timer")
}

func TestSignalSchedulerDefersWithinIntervalThenQuietFlush(t *testing.T) {
	h := newSchedulerHarness(10*time.Second, 2*time.Second)

	h.sched.markDirty("s1")
	require.Len(t, h.runsSnapshot(), 1)

	h.advance(1 * time.Second)
	h.sched.markDirty("s1")
	h.sched.tick()
	assert.Len(t, h.runsSnapshot(), 1,
		"mark within interval should defer, not recompute")

	h.advance(1 * time.Second)
	h.sched.tick()
	assert.Len(t, h.runsSnapshot(), 1,
		"tick before quiet delay elapses should not flush")

	h.advance(1 * time.Second)
	h.sched.tick()
	assert.Equal(t, []string{"s1", "s1"}, h.runsSnapshot(),
		"tick after quiet delay should flush the deferred recompute")

	h.sched.tick()
	assert.Len(t, h.runsSnapshot(), 2,
		"flushed session should not run again")
}

func TestSignalSchedulerContinuousMarksHonorInterval(t *testing.T) {
	h := newSchedulerHarness(10*time.Second, 2*time.Second)

	// Simulate a streaming session touching the scheduler every
	// 500ms for 12 seconds, ticking once per second.
	h.sched.markDirty("s1")
	for i := range 24 {
		h.advance(500 * time.Millisecond)
		h.sched.markDirty("s1")
		if i%2 == 1 {
			h.sched.tick()
		}
	}
	assert.Len(t, h.runsSnapshot(), 2,
		"continuous writes should recompute once per interval")

	h.advance(2 * time.Second)
	h.sched.tick()
	assert.Len(t, h.runsSnapshot(), 3,
		"trailing flush should run after writes go quiet")
}

func TestSignalSchedulerSessionsIndependent(t *testing.T) {
	h := newSchedulerHarness(10*time.Second, 2*time.Second)

	h.sched.markDirty("a")
	h.sched.markDirty("b")

	assert.Equal(t, []string{"a", "b"}, h.runsSnapshot(),
		"distinct sessions should not throttle each other")
}

func TestSignalSchedulerFlushAllRunsEverythingPending(t *testing.T) {
	h := newSchedulerHarness(10*time.Second, 2*time.Second)

	h.sched.markDirty("a")
	h.sched.markDirty("b")
	h.advance(1 * time.Second)
	h.sched.markDirty("a")
	h.sched.markDirty("b")
	require.Len(t, h.runsSnapshot(), 2, "second marks should defer")

	h.sched.flushAll()
	assert.ElementsMatch(t, []string{"a", "b", "a", "b"}, h.runsSnapshot(),
		"flushAll should recompute every pending session")

	h.advance(10 * time.Second)
	h.sched.tick()
	assert.Len(t, h.runsSnapshot(), 4,
		"nothing should remain pending after flushAll")
}

func TestSignalSchedulerTimerArmsOncePerBurstAndRearms(t *testing.T) {
	h := newSchedulerHarness(10*time.Second, 2*time.Second)

	h.sched.markDirty("s1")
	require.Zero(t, h.armedCount())

	h.advance(1 * time.Second)
	h.sched.markDirty("s1")
	require.Equal(t, 1, h.armedCount(),
		"first deferral should arm the flush timer")
	h.sched.markDirty("s1")
	require.Equal(t, 1, h.armedCount(),
		"further deferrals must not stack timers")

	// Timer fires before the quiet delay has elapsed: nothing
	// flushes, but the timer must re-arm to cover the still-dirty
	// session.
	h.fireTimer(t)
	assert.Len(t, h.runsSnapshot(), 1)
	require.Equal(t, 1, h.armedCount(),
		"early fire with dirty sessions should re-arm")

	h.advance(2 * time.Second)
	h.fireTimer(t)
	assert.Len(t, h.runsSnapshot(), 2,
		"fire after quiet delay should flush")
	assert.Zero(t, h.armedCount(),
		"no dirty sessions left, timer should stay disarmed")
}

func TestSignalSchedulerStopFlushesAndRunsInlineAfter(t *testing.T) {
	h := newSchedulerHarness(10*time.Second, 2*time.Second)

	h.sched.markDirty("s1")
	h.advance(1 * time.Second)
	h.sched.markDirty("s1")
	require.Len(t, h.runsSnapshot(), 1)

	require.Equal(t, 1, h.armedCount(),
		"deferral should have armed the flush timer")
	h.sched.stop()
	assert.Len(t, h.runsSnapshot(), 2,
		"stop should flush pending recomputes")
	assert.Zero(t, h.armedCount(),
		"stop should cancel the pending flush timer")

	h.sched.markDirty("s1")
	assert.Len(t, h.runsSnapshot(), 3,
		"marks after stop should recompute inline")
	assert.Zero(t, h.armedCount(),
		"stopped scheduler must not arm new timers")

	h.sched.stop() // double-stop must not panic
}

// TestSignalSchedulerStopWaitsForInflightTimerRun guards the
// shutdown ordering Engine.Close relies on: a recompute already
// running on the timer goroutine must finish before stop returns,
// or the owner could close the DB underneath it.
func TestSignalSchedulerStopWaitsForInflightTimerRun(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var mu sync.Mutex
	clock := time.Unix(1_700_000_000, 0)
	sched := newSignalScheduler(10*time.Second, 2*time.Second,
		func(string) {},
		func(flush func()) {
			startedOnce.Do(func() { close(started) })
			<-release
			flush()
		},
	)
	sched.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	var timerCB func()
	sched.afterFunc = func(_ time.Duration, f func()) func() bool {
		timerCB = f
		return func() bool { return false }
	}

	sched.markDirty("s1") // leading edge, inline
	sched.markDirty("s1") // defers and arms the timer
	require.NotNil(t, timerCB, "deferral should arm the flush timer")

	// The timer fires after the quiet delay and the deferred run
	// blocks, simulating a recompute in the middle of DB work.
	mu.Lock()
	clock = clock.Add(3 * time.Second)
	mu.Unlock()
	go timerCB()
	<-started

	stopDone := make(chan struct{})
	go func() {
		sched.stop()
		close(stopDone)
	}()
	stopped := func() bool {
		select {
		case <-stopDone:
			return true
		default:
			return false
		}
	}
	assert.Never(t, stopped, 100*time.Millisecond, 10*time.Millisecond,
		"stop must wait for the in-flight timer recompute")
	close(release)
	require.Eventually(t, stopped, 2*time.Second, 5*time.Millisecond,
		"stop must return once the in-flight recompute finishes")
}

// TestSignalSchedulerFlushAllInlineUsesInlinePath covers the flush
// variant for callers already holding the engine's sync lock: it
// must recompute pending sessions via the inline callback, never
// the deferred (lock-taking) one, and leave the scheduler running.
func TestSignalSchedulerFlushAllInlineUsesInlinePath(t *testing.T) {
	h := newSchedulerHarness(10*time.Second, 2*time.Second)

	h.sched.markDirty("s1")
	h.advance(1 * time.Second)
	h.sched.markDirty("s1")
	require.Len(t, h.runsSnapshot(), 1, "second mark should defer")

	h.sched.flushAllInline()
	assert.Equal(t, []string{"s1", "s1"}, h.runsSnapshot(),
		"inline flush should recompute every pending session")
	assert.Empty(t, h.deferredSnapshot(),
		"inline flush must not use the deferred path")

	h.advance(1 * time.Second)
	h.sched.markDirty("s1")
	assert.Len(t, h.runsSnapshot(), 2,
		"scheduler must keep debouncing after an inline flush")
}

// TestSignalSchedulerRoutesDeferredRunsSeparately pins which runs
// happen inside the exclusive section (timer ticks, flushes) versus
// directly (leading edge, stopped pass-through). The engine takes
// the sync lock only in exclusive, so misrouting either way would
// mean recomputes racing sync writes or a self-deadlock.
func TestSignalSchedulerRoutesDeferredRunsSeparately(t *testing.T) {
	h := newSchedulerHarness(10*time.Second, 2*time.Second)

	h.sched.markDirty("s1")
	assert.Empty(t, h.deferredSnapshot(),
		"leading-edge run must use the inline path")

	h.advance(1 * time.Second)
	h.sched.markDirty("s1")
	h.advance(2 * time.Second)
	h.sched.tick()
	assert.Equal(t, []string{"s1"}, h.deferredSnapshot(),
		"tick flushes must use the deferred path")

	h.advance(1 * time.Second)
	h.sched.markDirty("s1")
	h.sched.flushAll()
	assert.Equal(t, []string{"s1", "s1"}, h.deferredSnapshot(),
		"flushAll must use the deferred path")

	h.sched.stop()
	h.sched.markDirty("s1")
	assert.Equal(t, []string{"s1", "s1"}, h.deferredSnapshot(),
		"stopped pass-through marks must stay inline")
	assert.Len(t, h.runsSnapshot(), 4,
		"every mark and flush must still recompute exactly once")
}

// TestDeferredSignalRecomputeSerializesWithSync verifies the engine
// wires deferred recomputes through syncMu: a flush racing an
// in-flight sync must wait for it, so a delayed recompute can never
// read an older message snapshot and then overwrite signals written
// by that sync.
func TestDeferredSignalRecomputeSerializesWithSync(t *testing.T) {
	fx := newEngineFixture(t)
	e := fx.engine

	// First mark runs inline; the second defers so the flush below
	// has pending work.
	e.signalSched.markDirty("sess-lock")
	e.signalSched.markDirty("sess-lock")

	e.syncMu.Lock()
	done := make(chan struct{})
	go func() {
		e.signalSched.flushAll()
		close(done)
	}()
	flushed := func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	assert.Never(t, flushed, 100*time.Millisecond, 10*time.Millisecond,
		"deferred recompute must wait for the in-flight sync operation")
	e.syncMu.Unlock()
	require.Eventually(t, flushed, 2*time.Second, 5*time.Millisecond,
		"deferred recompute must run once the sync lock is released")
}

// TestLockedFlushSeesSessionClaimedByBlockedTimer reproduces the
// claim-then-block race: the flush timer fires while a sync holds
// syncMu. The timer must not claim sessions out of the dirty map
// before it can recompute under the lock — otherwise the sync's own
// pre-push flush (flushAllInline in SyncThenRun) sees no pending
// work and pushes stale signal fields.
func TestLockedFlushSeesSessionClaimedByBlockedTimer(t *testing.T) {
	fx := newEngineFixture(t)
	e := fx.engine

	// Capture the flush timer instead of arming a real one.
	var timerCB func()
	e.signalSched.afterFunc = func(_ time.Duration, f func()) func() bool {
		timerCB = f
		return func() bool { return false }
	}

	path := fx.writeClaudeSession(t, "proj", "sig-race.jsonl", "hello")
	e.SyncAll(context.Background(), nil)
	sid := fx.sessionIDFor(t, path)

	fx.appendClaudeMessage(t, path, "key AKIA7QHWN2DKR4FYPLJM leaked")
	e.SyncPaths([]string{path})
	require.Equal(t, 1, secretLeakCount(t, fx, sid))
	fx.appendClaudeMessage(t, path, "key AKIA9XKQV3ZTN8WMB2RC leaked")
	e.SyncPaths([]string{path})
	require.Equal(t, 1, secretLeakCount(t, fx, sid),
		"second write within interval should defer the recompute")
	require.NotNil(t, timerCB, "deferral should arm the flush timer")

	// The quiet delay has elapsed by the time the timer fires, so
	// its takeDue pass considers the session due.
	e.signalSched.now = func() time.Time {
		return time.Now().Add(3 * time.Second)
	}

	// A sync is in progress when the timer fires.
	e.syncMu.Lock()
	timerDone := make(chan struct{})
	go func() {
		timerCB()
		close(timerDone)
	}()
	// Give the timer goroutine time to reach the lock (and, in the
	// buggy ordering, to claim the session before blocking).
	time.Sleep(50 * time.Millisecond)

	// The sync now flushes before its push work, as SyncThenRun does.
	e.signalSched.flushAllInline()
	seen := secretLeakCount(t, fx, sid)
	e.syncMu.Unlock()

	assert.Equal(t, 2, seen,
		"locked flush must recompute sessions the blocked timer would handle")
	require.Eventually(t, func() bool {
		select {
		case <-timerDone:
			return true
		default:
			return false
		}
	}, 2*time.Second, 5*time.Millisecond, "timer callback must finish")
}

// secretLeakCount reads the session's persisted secret_leak_count
// signal, the observable for whether a recompute has run.
func secretLeakCount(t *testing.T, fx *engineFixture, sessionID string) int {
	t.Helper()
	sess, err := fx.db.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err, "GetSessionFull")
	require.NotNil(t, sess, "session %s not found", sessionID)
	return sess.SecretLeakCount
}

func TestWriteIncrementalDebouncesSignalRecompute(t *testing.T) {
	fx := newEngineFixture(t)
	path := fx.writeClaudeSession(t, "proj", "sig-debounce.jsonl", "hello")
	fx.engine.SyncAll(context.Background(), nil)
	sid := fx.sessionIDFor(t, path)
	require.Zero(t, secretLeakCount(t, fx, sid))

	// First incremental append is the session's first mark, so the
	// leading edge recomputes inline and the new secret is counted
	// immediately.
	fx.appendClaudeMessage(t, path, "key AKIA7QHWN2DKR4FYPLJM leaked")
	fx.engine.SyncPaths([]string{path})
	require.Equal(t, 1, secretLeakCount(t, fx, sid),
		"first incremental write should recompute signals inline")

	// A second append inside the debounce interval must defer the
	// recompute: the stored signal stays stale until a flush.
	fx.appendClaudeMessage(t, path, "key AKIA9XKQV3ZTN8WMB2RC leaked")
	fx.engine.SyncPaths([]string{path})
	assert.Equal(t, 1, secretLeakCount(t, fx, sid),
		"second write within interval should defer the recompute")

	fx.engine.FlushSignals()
	assert.Equal(t, 2, secretLeakCount(t, fx, sid),
		"flush should persist the deferred recompute")

	// FlushSignals must leave the scheduler running: another write
	// inside the interval defers again instead of running inline.
	fx.appendClaudeMessage(t, path, "key AKIA2PLVWX6QR8ZKN4TJ leaked")
	fx.engine.SyncPaths([]string{path})
	assert.Equal(t, 2, secretLeakCount(t, fx, sid),
		"scheduler must keep debouncing after FlushSignals")
	fx.engine.FlushSignals()
	assert.Equal(t, 3, secretLeakCount(t, fx, sid),
		"third secret must flush, proving the deferral was real")
}

// TestSyncThenRunFlushesSignalsBeforeWork mirrors the PG/DuckDB push
// endpoints: work scans SQLite rows while syncMu is held, so any
// deferred signal recompute must be flushed before work runs or the
// push carries stale fields such as secret_leak_count.
func TestSyncThenRunFlushesSignalsBeforeWork(t *testing.T) {
	fx := newEngineFixture(t)
	path := fx.writeClaudeSession(t, "proj", "sig-flush.jsonl", "hello")
	fx.engine.SyncAll(context.Background(), nil)
	sid := fx.sessionIDFor(t, path)

	fx.appendClaudeMessage(t, path, "key AKIA7QHWN2DKR4FYPLJM leaked")
	fx.engine.SyncPaths([]string{path})
	require.Equal(t, 1, secretLeakCount(t, fx, sid))
	fx.appendClaudeMessage(t, path, "key AKIA9XKQV3ZTN8WMB2RC leaked")
	fx.engine.SyncPaths([]string{path})
	require.Equal(t, 1, secretLeakCount(t, fx, sid),
		"second write within interval should defer the recompute")

	var seen int
	_, err := fx.engine.SyncThenRun(context.Background(), false, nil,
		func(bool) error {
			seen = secretLeakCount(t, fx, sid)
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, 2, seen,
		"work must observe flushed signal fields before pushing")
}

func TestSignalSchedulerRealTimerFlushes(t *testing.T) {
	var mu sync.Mutex
	var runs int
	run := func(string) {
		mu.Lock()
		runs++
		mu.Unlock()
	}
	sched := newSignalScheduler(
		50*time.Millisecond, 10*time.Millisecond, run,
		func(flush func()) { flush() },
	)

	sched.markDirty("s1")
	sched.markDirty("s1")
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return runs == 2
	}, 2*time.Second, 5*time.Millisecond,
		"deferred recompute should flush via the real timer")
}
