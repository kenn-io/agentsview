package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/vector"
)

// --- fake embedManager ---

// fakeEmbedManager records every TryBuild call and returns scripted
// (started, err) results in order, repeating the last scripted result once
// the script is exhausted (default: started=true, err=nil).
type fakeEmbedManager struct {
	mu      sync.Mutex
	calls   []vector.BuildRequest
	results []fakeTryBuildResult
}

type fakeTryBuildResult struct {
	started bool
	err     error
}

type blockingEmbedManager struct {
	started     chan struct{}
	startedOnce sync.Once
	release     chan struct{}
	releasedFn  sync.Once
}

func (b *blockingEmbedManager) TryBuild(
	_ context.Context, _ vector.BuildRequest,
) (bool, error) {
	b.startedOnce.Do(func() { close(b.started) })
	<-b.release
	return true, nil
}

func (b *blockingEmbedManager) releaseOnce() {
	b.releasedFn.Do(func() { close(b.release) })
}

func (f *fakeEmbedManager) TryBuild(
	_ context.Context, req vector.BuildRequest,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	idx := len(f.calls) - 1
	if idx < len(f.results) {
		r := f.results[idx]
		return r.started, r.err
	}
	if len(f.results) > 0 {
		r := f.results[len(f.results)-1]
		return r.started, r.err
	}
	return true, nil
}

func (f *fakeEmbedManager) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeEmbedManager) callsSnapshot() []vector.BuildRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vector.BuildRequest(nil), f.calls...)
}

// waitForSchedulerCondition polls cond until it is true or 2s pass, failing
// the test with msg otherwise. Avoids fixed sleeps that would either flake
// under load or slow the suite down needlessly.
func waitForSchedulerCondition(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	waitForSchedulerConditionWithin(t, 2*time.Second, cond, msg)
}

func waitForSchedulerConditionWithin(
	t *testing.T, timeout time.Duration, cond func() bool, msg string,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.Fail(t, "timed out waiting for condition", msg)
}

func TestEmbedSchedulerBurstOfNotifyProducesExactlyOneBuild(t *testing.T) {
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, 20*time.Millisecond, 0, false, nil)

	// Queue the whole burst before Run starts so the test exercises the
	// scheduler's documented pre-reader coalescing without racing the debounce
	// interval on slow or coarsely scheduled runners.
	for range 10 {
		s.Notify()
	}

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"expected a build after the burst quieted")
	// Give any spurious extra build a chance to show up before asserting
	// there is exactly one.
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, 1, fake.callCount(), "a burst of Notify must collapse to one build")
	assert.Equal(t, []vector.BuildRequest{{}}, fake.callsSnapshot())
}

// TestEmbedSchedulerIncludeAutomatedThreadsIntoBuildRequests asserts the
// scheduler's configured include-automated scope rides along on every
// BuildRequest it issues -- both the debounced after-sync path and the
// backstop ticker -- so scheduled builds stay config-authoritative rather
// than silently reverting to includeAutomated=false.
func TestEmbedSchedulerIncludeAutomatedThreadsIntoBuildRequests(t *testing.T) {
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, 5*time.Millisecond, 20*time.Millisecond, true, nil)

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	s.Notify()
	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"expected a debounced build")

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 2 },
		"expected a backstop build")

	for _, req := range fake.callsSnapshot() {
		assert.True(t, req.IncludeAutomated,
			"every scheduler-issued BuildRequest must carry the configured scope")
	}
}

func TestEmbedSchedulerNotifyDuringRunningBuildRearmsForFollowUpPass(t *testing.T) {
	fake := &fakeEmbedManager{
		results: []fakeTryBuildResult{
			{started: false, err: nil}, // a build is already running elsewhere
			{started: true, err: nil},  // the follow-up pass actually runs
		},
	}
	s := newEmbedScheduler(fake, 15*time.Millisecond, 0, false, nil)

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	s.Notify()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 2 },
		"expected the scheduler to re-arm and retry after a dropped build")
	calls := fake.callsSnapshot()
	require.Len(t, calls, 2)
	assert.Equal(t, vector.BuildRequest{}, calls[0])
	assert.Equal(t, vector.BuildRequest{}, calls[1])
}

func TestEmbedSchedulerBackstopTickIssuesBackstopBuild(t *testing.T) {
	fake := &fakeEmbedManager{}
	// A very long debounce so only the backstop ticker can fire a build.
	s := newEmbedScheduler(fake, time.Hour, 20*time.Millisecond, false, nil)

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"expected a backstop build")
	calls := fake.callsSnapshot()
	require.NotEmpty(t, calls)
	assert.True(t, calls[0].Backstop, "backstop tick must set BuildRequest.Backstop")
}

func TestEmbedSchedulerPendingDebounceHoldsIdleWorkLease(t *testing.T) {
	fake := &fakeEmbedManager{}
	idled := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	tracker := server.NewIdleTracker(20*time.Millisecond, func() {
		close(idled)
		cancel()
	})
	s := newEmbedScheduler(fake, 100*time.Millisecond, 0, false, tracker)

	// setupVectorServing queues Recall startup work before either goroutine
	// starts, so exercise that ordering explicitly.
	s.Notify()
	go tracker.Run(ctx)
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"pending startup work did not survive an idle timeout shorter than its debounce")
	select {
	case <-idled:
	case <-time.After(2 * time.Second):
		require.Fail(t, "daemon never idled after the pending build completed")
	}
}

func TestEmbedSchedulerBuildsHoldIdleWorkLease(t *testing.T) {
	tests := []struct {
		name     string
		debounce time.Duration
		backstop time.Duration
		notify   bool
	}{
		{name: "debounced", debounce: 5 * time.Millisecond, notify: true},
		{name: "backstop", debounce: time.Hour, backstop: 5 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &blockingEmbedManager{
				started: make(chan struct{}),
				release: make(chan struct{}),
			}
			idled := make(chan struct{})
			tracker := server.NewIdleTracker(50*time.Millisecond, func() { close(idled) })
			s := newEmbedScheduler(
				mgr, tt.debounce, tt.backstop, false, tracker,
			)
			ctx := t.Context()
			go tracker.Run(ctx)
			go s.Run(ctx)
			defer s.Stop()
			defer mgr.releaseOnce()

			if tt.notify {
				s.Notify()
			}
			<-mgr.started
			select {
			case <-idled:
				require.Fail(t, "daemon idled while an embedding build was in flight")
			case <-time.After(200 * time.Millisecond):
			}
			mgr.releaseOnce()
			select {
			case <-idled:
			case <-time.After(2 * time.Second):
				require.Fail(t, "daemon never idled after the embedding build completed")
			}
		})
	}
}

func TestEmbedSchedulerFailedDebouncedBuildRetriesWithoutNotification(
	t *testing.T,
) {
	fake := &fakeEmbedManager{results: []fakeTryBuildResult{
		{started: true, err: errors.New("embedding endpoint unavailable")},
		{started: true},
	}}
	idled := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	tracker := server.NewIdleTracker(20*time.Millisecond, func() {
		close(idled)
		cancel()
	})
	s := newEmbedScheduler(fake, 50*time.Millisecond, 0, false, tracker)
	s.Notify()
	go tracker.Run(ctx)
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 2 },
		"a failed debounced build must retry without another notification")
	select {
	case <-idled:
	case <-time.After(2 * time.Second):
		require.Fail(t, "daemon never idled after the retry succeeded")
	}
	assert.Equal(t, []vector.BuildRequest{{}, {}}, fake.callsSnapshot())
}

func TestEmbedSchedulerRepeatedDebouncedFailuresReleaseIdleLease(
	t *testing.T,
) {
	buildErr := errors.New("embedding request rejected")
	fake := &fakeEmbedManager{results: []fakeTryBuildResult{
		{started: true, err: buildErr},
		{started: true, err: buildErr},
	}}
	idled := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	tracker := server.NewIdleTracker(15*time.Millisecond, func() {
		close(idled)
		cancel()
	})
	s := newEmbedScheduler(fake, 20*time.Millisecond, 0, false, tracker)
	s.Notify()
	go tracker.Run(ctx)
	go s.Run(ctx)
	defer s.Stop()

	select {
	case <-idled:
	case <-time.After(2 * time.Second):
		require.Fail(t, "repeated build failures kept the daemon alive indefinitely")
	}
	assert.Equal(t, []vector.BuildRequest{{}, {}}, fake.callsSnapshot(),
		"one notification should get one bounded retry")
}

func TestEmbedSchedulerPendingBackstopRetriesBeforeNextTick(
	t *testing.T,
) {
	tests := []struct {
		name   string
		result fakeTryBuildResult
	}{
		{
			name: "failed build",
			result: fakeTryBuildResult{
				started: true, err: errors.New("embedding endpoint unavailable"),
			},
		},
		{name: "colliding build", result: fakeTryBuildResult{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeEmbedManager{results: []fakeTryBuildResult{
				tt.result,
				{started: true},
			}}
			idled := make(chan struct{})
			ctx, cancel := context.WithCancel(t.Context())
			tracker := server.NewIdleTracker(450*time.Millisecond, func() {
				close(idled)
				cancel()
			})
			s := newEmbedScheduler(
				fake, 200*time.Millisecond, 300*time.Millisecond, false, tracker,
			)
			go tracker.Run(ctx)
			go s.Run(ctx)
			defer s.Stop()

			waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
				"expected the initial backstop attempt")
			waitForSchedulerConditionWithin(t, 250*time.Millisecond,
				func() bool { return fake.callCount() >= 2 },
				"a pending backstop must retry before the next backstop tick")
			select {
			case <-idled:
			case <-time.After(2 * time.Second):
				require.Fail(t, "daemon never idled after the backstop retry succeeded")
			}
			calls := fake.callsSnapshot()
			require.Len(t, calls, 2)
			assert.True(t, calls[0].Backstop)
			assert.True(t, calls[1].Backstop)
		})
	}
}

func TestEmbedSchedulerRepeatedBackstopFailuresReleaseIdleLease(
	t *testing.T,
) {
	buildErr := errors.New("embedding request rejected")
	fake := &fakeEmbedManager{results: []fakeTryBuildResult{
		{started: true, err: buildErr},
		{started: true, err: buildErr},
	}}
	idled := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	tracker := server.NewIdleTracker(30*time.Millisecond, func() {
		close(idled)
		cancel()
	})
	s := newEmbedScheduler(
		fake, 20*time.Millisecond, 200*time.Millisecond, false, tracker,
	)
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 2 },
		"expected the failed backstop and its bounded retry")
	// Start idle observation only after the backstop has acquired its lease;
	// otherwise a detached daemon with no startup work could reap before the
	// first deliberately delayed test tick.
	go tracker.Run(ctx)
	select {
	case <-idled:
	case <-time.After(150 * time.Millisecond):
		require.Fail(t, "repeated backstop failures retained the idle lease")
	}
	assert.Equal(t, []vector.BuildRequest{{Backstop: true}, {Backstop: true}},
		fake.callsSnapshot(), "one backstop tick should get one bounded retry")
}

func TestEmbedSchedulerExhaustedBackstopCarriesIntoNextNotification(
	t *testing.T,
) {
	buildErr := errors.New("embedding request rejected")
	fake := &fakeEmbedManager{results: []fakeTryBuildResult{
		{started: true, err: buildErr},
		{started: true, err: buildErr},
		{started: true},
	}}
	s := newEmbedScheduler(
		fake, 20*time.Millisecond, 200*time.Millisecond, false, nil,
	)
	go s.Run(t.Context())
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 2 },
		"expected the failed backstop and its bounded retry")
	s.Notify()
	waitForSchedulerConditionWithin(t, 100*time.Millisecond,
		func() bool { return fake.callCount() >= 3 },
		"a new notification should recover the deferred reconciliation")

	assert.Equal(t, []vector.BuildRequest{
		{Backstop: true}, {Backstop: true}, {Backstop: true},
	}, fake.callsSnapshot(),
		"retry exhaustion must preserve full-reconciliation intent")
}

// TestEmbedSchedulerDroppedBackstopRetriesOnNextDebouncedBuild is the fix-4
// regression test: a backstop tick that collides with a build already
// running elsewhere must not be silently dropped for a full backstop
// interval (24h in production). The scheduler must remember it and fold
// Backstop: true into the next debounced build request instead.
func TestEmbedSchedulerDroppedBackstopRetriesOnNextDebouncedBuild(t *testing.T) {
	fake := &fakeEmbedManager{
		results: []fakeTryBuildResult{
			{started: false, err: nil}, // the backstop tick collides with a build elsewhere
			{started: true, err: nil},  // the following debounced build recovers it
		},
	}
	// A long backstop interval relative to the debounce interval and the
	// test's own buffers keeps a second, unrelated backstop tick from
	// firing mid-test and making the call count non-deterministic.
	s := newEmbedScheduler(fake, 10*time.Millisecond, 500*time.Millisecond, false, nil)

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"expected the backstop tick to fire and be dropped")

	s.Notify()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 2 },
		"expected the debounced build to recover the dropped backstop")
	time.Sleep(50 * time.Millisecond)

	calls := fake.callsSnapshot()
	require.Len(t, calls, 2, "the dropped backstop must be retried exactly once, not repeatedly")
	assert.True(t, calls[0].Backstop, "the original (dropped) backstop tick request")
	assert.True(t, calls[1].Backstop,
		"the debounced build must carry the pending backstop forward instead of dropping it")

	// Once the recovered build actually started, the pending flag must
	// clear: a further, unrelated debounced build must not keep carrying
	// Backstop: true forever.
	s.Notify()
	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 3 },
		"expected a further debounced build after the recovered one")
	time.Sleep(50 * time.Millisecond)

	calls = fake.callsSnapshot()
	require.Len(t, calls, 3)
	assert.False(t, calls[2].Backstop,
		"the recovered backstop must not leak into a later unrelated debounced build")
}

// TestEmbedSchedulerBackstopTickStartedButFailedKeepsPendingBackstop is the
// fix-5 regression test: a backstop tick whose TryBuild call actually started
// but then returned an error must not clear pendingBackstop -- the
// reconciliation it carried never completed, so it must be retried on the
// very next debounced build rather than silently deferred to the next
// backstop interval (24h in production).
func TestEmbedSchedulerBackstopTickStartedButFailedKeepsPendingBackstop(t *testing.T) {
	buildErr := errors.New("embeddings endpoint unreachable")
	fake := &fakeEmbedManager{
		results: []fakeTryBuildResult{
			{started: true, err: buildErr}, // the backstop tick starts but fails
			{started: true, err: nil},      // the following debounced build recovers it
		},
	}
	s := newEmbedScheduler(fake, 10*time.Millisecond, 500*time.Millisecond, false, nil)

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"expected the backstop tick to fire and fail")

	s.Notify()
	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 2 },
		"expected the debounced build to retry the failed backstop")
	time.Sleep(50 * time.Millisecond)

	calls := fake.callsSnapshot()
	require.Len(t, calls, 2, "the failed backstop must be retried exactly once, not repeatedly")
	assert.True(t, calls[0].Backstop, "the original (started-but-failed) backstop tick request")
	assert.True(t, calls[1].Backstop,
		"the debounced build must carry the failed backstop forward instead of dropping it")

	// Once the recovered build actually succeeded, the pending flag must
	// clear: a further, unrelated debounced build must not keep carrying
	// Backstop: true forever.
	s.Notify()
	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 3 },
		"expected a further debounced build after the recovered one")
	time.Sleep(50 * time.Millisecond)

	calls = fake.callsSnapshot()
	require.Len(t, calls, 3)
	assert.False(t, calls[2].Backstop,
		"the recovered backstop must not leak into a later unrelated debounced build")
}

// TestEmbedSchedulerDebouncedBuildStartedButFailedKeepsPendingBackstop is the
// fix-5 regression test for the debounced-build path: once a dropped
// backstop tick is being carried by pendingBackstop, a debounced build that
// starts but then fails must not clear it either -- the same
// started-but-failed rule applies on both paths that can clear the flag.
func TestEmbedSchedulerDebouncedBuildStartedButFailedKeepsPendingBackstop(t *testing.T) {
	buildErr := errors.New("embeddings endpoint unreachable")
	fake := &fakeEmbedManager{
		results: []fakeTryBuildResult{
			{started: false, err: nil},     // the backstop tick collides with a build elsewhere
			{started: true, err: buildErr}, // the recovering debounced build starts but fails
			{started: true, err: nil},      // a further debounced build finally succeeds
		},
	}
	s := newEmbedScheduler(fake, 10*time.Millisecond, 500*time.Millisecond, false, nil)

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"expected the backstop tick to fire and be dropped")

	s.Notify()
	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 2 },
		"expected the debounced build to attempt recovering the dropped backstop")

	s.Notify()
	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 3 },
		"expected a further debounced build to retry after the recovering build failed")
	time.Sleep(50 * time.Millisecond)

	calls := fake.callsSnapshot()
	require.Len(t, calls, 3)
	assert.True(t, calls[0].Backstop, "the original (dropped) backstop tick request")
	assert.True(t, calls[1].Backstop, "the recovering (started-but-failed) debounced build")
	assert.True(t, calls[2].Backstop,
		"a started-but-failed build must not clear pendingBackstop: the retry must still carry it")
}

func TestEmbedSchedulerStopTerminatesRun(t *testing.T) {
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, time.Hour, 0, false, nil)

	go s.Run(context.Background())

	// Stop blocks until Run has actually exited, so its returning at all
	// (within a generous timeout) is the proof Run terminated.
	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		require.Fail(t, "Stop did not return; Run likely never terminated")
	}
}

func TestEmbedSchedulerNotifyNeverBlocksWithoutAReader(t *testing.T) {
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, time.Hour, 0, false, nil)

	done := make(chan struct{})
	go func() {
		for range 100 {
			s.Notify()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "Notify blocked with no Run consuming dirty")
	}
}

// --- teeEmitter ---

type recordingEmitter struct {
	mu     sync.Mutex
	scopes []string
}

func (e *recordingEmitter) Emit(scope string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scopes = append(e.scopes, scope)
}

func (e *recordingEmitter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.scopes)
}

func TestTeeEmitterAlwaysCallsPrimaryAndGatesSchedulerOnRunAfterSync(t *testing.T) {
	primary := &recordingEmitter{}
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, 10*time.Millisecond, 0, false, nil)
	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	disabled := teeEmitter{primary: primary, scheduler: s, runAfterSync: false}
	disabled.Emit("sessions")
	assert.Equal(t, 1, primary.count())
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, 0, fake.callCount(), "runAfterSync=false must not notify the scheduler")

	enabled := teeEmitter{primary: primary, scheduler: s, runAfterSync: true}
	enabled.Emit("sessions")
	assert.Equal(t, 2, primary.count())
	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"runAfterSync=true must notify the scheduler")
}

// TestRunRemoteHostSyncLoop_EmitsThroughTeeNotifiesScheduler is a
// regression test for a bug where runServe wired startPeriodicSync's
// scheduled remote-host sync path to the bare SSE broadcaster instead of
// the wrapped teeEmitter, so remote-synced sessions notified SSE clients
// but never reached the embed scheduler until an unrelated local sync or
// the 24h backstop ran. It drives runRemoteHostSyncLoop — the function
// startPeriodicSync spawns per configured remote host — with a teeEmitter
// and a syncFn reporting synced sessions, and asserts the scheduler
// actually receives a build trigger through that path.
func TestRunRemoteHostSyncLoop_EmitsThroughTeeNotifiesScheduler(t *testing.T) {
	primary := &recordingEmitter{}
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, 5*time.Millisecond, 0, false, nil)
	schedCtx := t.Context()
	go s.Run(schedCtx)
	defer s.Stop()

	tee := teeEmitter{primary: primary, scheduler: s, runAfterSync: true}

	syncFn := func() (int, error) {
		return 1, nil // one session synced on this remote host
	}

	loopCtx := t.Context()
	done := make(chan struct{})
	go runRemoteHostSyncLoop(
		loopCtx, "remote-host", 5*time.Millisecond, syncFn, tee, nil, done,
	)
	defer close(done)

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"a scheduled remote sync reaching the tee emitter must notify the embed scheduler")
	assert.NotZero(t, primary.count(),
		"the tee must still forward remote sync completions to the SSE broadcaster")
}

// --- searcherAdapter error taxonomy ---

func TestTranslateSearchErrorMapsVectorErrorsToSemanticUnavailable(t *testing.T) {
	t.Run("no active generation", func(t *testing.T) {
		err := translateSearchError(vector.ErrNoActiveGeneration)
		assert.ErrorIs(t, err, db.ErrSemanticUnavailable)
	})
	t.Run("building", func(t *testing.T) {
		err := translateSearchError(&vector.BuildingError{Percent: 62})
		assert.ErrorIs(t, err, db.ErrSemanticUnavailable)
		assert.Contains(t, err.Error(), "index is building: 62% complete")
	})
	t.Run("other error passes through", func(t *testing.T) {
		boom := errors.New("boom")
		assert.Same(t, boom, translateSearchError(boom))
	})
	t.Run("query encode failure maps to semantic transient, not unavailable", func(t *testing.T) {
		queryErr := &vector.QueryEncodeError{Err: errors.New("dial tcp: connection refused")}
		got := translateSearchError(queryErr)
		assert.ErrorIs(t, got, db.ErrSemanticTransient)
		assert.False(t, errors.Is(got, db.ErrSemanticUnavailable),
			"a query-time endpoint failure must not read as semantic search being disabled")
		assert.Contains(t, got.Error(), "connection refused")
	})
	t.Run("query encode failure preserves the underlying cause chain", func(t *testing.T) {
		queryErr := &vector.QueryEncodeError{
			Err: fmt.Errorf("encoding query: %w", context.Canceled),
		}
		got := translateSearchError(queryErr)
		assert.ErrorIs(t, got, db.ErrSemanticTransient)
		assert.ErrorIs(t, got, context.Canceled,
			"context errors must stay matchable so cancellation handling still fires")
	})
	t.Run("mirror version mismatch maps to semantic unavailable with rebuild message", func(t *testing.T) {
		got := translateSearchError(
			fmt.Errorf("checking embedding index staleness: %w", vector.ErrMirrorVersionMismatch))
		assert.ErrorIs(t, got, db.ErrSemanticUnavailable)
		assert.Contains(t, got.Error(), "embeddings build",
			"the rebuild remediation must survive translation")
	})
}

func TestTranslateRecallSearchErrorUsesRecallRemediation(t *testing.T) {
	t.Run("mirror version mismatch", func(t *testing.T) {
		err := translateRecallSearchError(vector.ErrMirrorVersionMismatch)

		assert.ErrorIs(t, err, db.ErrSemanticUnavailable)
		assert.Contains(t, err.Error(), "embeddings build --store recall")
	})
	t.Run("building", func(t *testing.T) {
		err := translateRecallSearchError(&vector.BuildingError{Percent: 37})

		assert.ErrorIs(t, err, db.ErrSemanticUnavailable)
		assert.Contains(t, err.Error(), "recall index is building: 37% complete")
		assert.NotContains(t, err.Error(), "agentsview embeddings build",
			"an in-progress Recall build should not recommend another store build")
	})
}

func TestRecallSearcherNoActiveGenerationNamesRecallBuild(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	cfg := vectorTestConfig(dataDir)
	ix, err := vector.OpenSpec(
		t.Context(), cfg.Vector.ResolvedDBPath(dataDir),
		vector.RecallIndexSpec(), false, cfg.Vector.Embeddings.MaxInputChars,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	searcher := recallSearcherAdapter{ix: ix, database: database, cfg: cfg}

	_, _, _, err = searcher.SearchRecall(t.Context(), "database pool", 5)

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrSemanticUnavailable)
	assert.Contains(t, err.Error(), "embeddings build --store recall")
}

// TestSearcherAdapterVersionMismatchedIndexReturnsSemanticUnavailable is the
// adapter-level regression test for the mirror version gate: a
// searcherAdapter over a read-only vectors.db written by a different mirror
// schema version must return an error matching db.ErrSemanticUnavailable and
// mentioning the rebuild remediation — not a raw SQL error or a wrong
// staleness verdict from StaleActive querying an incompatible mirror.
func TestSearcherAdapterVersionMismatchedIndexReturnsSemanticUnavailable(t *testing.T) {
	dataDir := t.TempDir()
	cfg := vectorTestConfig(dataDir)
	path := cfg.Vector.ResolvedDBPath(dataDir)

	// Create a current vectors.db, then restamp it as written by the
	// previous mirror schema version, simulating a file left behind by an
	// older agentsview build.
	seed, err := vector.Open(context.Background(), path, false, cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	require.NoError(t, seed.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.Exec(`UPDATE vector_meta SET value = '2' WHERE key = 'mirror_schema_version'`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	ix, err := vector.Open(context.Background(), path, true, cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err, "read-only Open must succeed against a mismatched vectors.db")
	defer ix.Close()

	enc, err := newVectorQueryEncoder(cfg.Vector.Embeddings, "")
	require.NoError(t, err)
	adapter := newSearcherAdapter(ix, enc, vectorGeneration(cfg.Vector.Embeddings))

	_, err = adapter.SemanticSearch(context.Background(), "any query", 5)
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrSemanticUnavailable,
		"a version-mismatched index must surface the semantic-unavailable taxonomy")
	assert.Contains(t, err.Error(), "embeddings build",
		"the error must tell the user to rebuild the index")
}

// --- integration: real serve/server construction path ---

// vectorTestConfig returns a config.Config with a small-but-valid [vector]
// section (accepted by config.Validate) pointed at an embeddings endpoint
// that is never actually called in these tests — setupVectorServing only
// constructs the encoder, it does not exercise it.
func vectorTestConfig(dataDir string) config.Config {
	return config.Config{
		DataDir: dataDir,
		DBPath:  filepath.Join(dataDir, "sessions.db"),
		Vector: config.VectorConfig{
			Enabled: true,
			Embeddings: config.VectorEmbeddingsConfig{
				Model:         "test-model",
				Dimension:     3,
				MaxInputChars: 1000,
				Servers: map[string]config.VectorEmbeddingsServerConfig{
					"local": {
						Endpoint:    "http://127.0.0.1:1/v1",
						BatchSize:   10,
						Concurrency: 1,
						Timeout:     "5s",
						MaxRetries:  1,
					},
				},
			},
			Embed: config.VectorEmbedConfig{
				BackstopInterval: "24h",
				Recall:           true,
			},
		},
	}
}

// listenLoopback opens a loopback listener on an OS-assigned port,
// returning it alongside the port so a caller can set cfg.Port to it
// before constructing the server: the host-check middleware validates
// the Host header against cfg.Host:cfg.Port exactly, so the request's
// destination port must be known up front rather than assigned by
// httptest.NewServer after the fact.
func listenLoopback(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return ln, ln.Addr().(*net.TCPAddr).Port
}

// startTestServer serves handler on ln (already bound to the port cfg.Port
// was set to) and returns an *httptest.Server whose URL and lifecycle
// helpers work as usual.
func startTestServer(t *testing.T, ln net.Listener, handler http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(handler)
	require.NoError(t, ts.Listener.Close())
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// TestServeConstructionRegistersEmbeddingsRoutesWhenVectorEnabled drives the
// real setupVectorServing + server.New construction path (not a fake mux)
// with a vector-enabled config and asserts the embeddings status endpoint
// responds, then a sibling test rebuilds with vector disabled and asserts
// it does not.
func TestServeConstructionRegistersEmbeddingsRoutesWhenVectorEnabled(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	ln, port := listenLoopback(t)
	cfg := vectorTestConfig(dataDir)
	cfg.Host, cfg.Port = "127.0.0.1", port
	vs, err := setupVectorServing(context.Background(), cfg, database, nil)
	require.NoError(t, err)
	require.NotNil(t, vs.Scheduler)
	require.NotNil(t, vs.RecallScheduler)
	require.NotNil(t, vs.Close)
	defer func() { require.NoError(t, vs.Close()) }()

	srv := server.New(cfg, database, nil, vs.ServerOpts...)
	ts := startTestServer(t, ln, srv.Handler())

	resp, err := http.Get(ts.URL + "/api/v1/embeddings/status")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", mediaType(t, resp.Header.Get("Content-Type")))
}

func TestRecallSchedulerBackstopRemovesArchivedEntryVectors(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSession(t, database, "s1", "agentsview")
	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO recall_extract_generations
				(fingerprint, state, model, segmenter, params_json)
			VALUES ('extract-active', 'active', 'extract-model', 'turns-v1', '{}')`)
		return err
	}))
	_, err := database.InsertRecallEntry(db.RecallEntry{
		ID: "recall-entry", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Database pool", Body: "Reuse idle connections.",
		SourceSessionID: "s1", SourceRunID: "extract-active",
	})
	require.NoError(t, err)

	stub := newEmbeddingsStubServer(t, 3)
	t.Cleanup(stub.Close)
	cfg := vectorTestConfig(dataDir)
	embeddingsServer := cfg.Vector.Embeddings.Servers["local"]
	embeddingsServer.Endpoint = stub.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = embeddingsServer
	cfg.Vector.Embed.BackstopInterval = "20ms"

	vs, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(t, err)
	require.NotNil(t, vs.RecallScheduler)
	require.NotNil(t, vs.Close)
	go vs.RecallScheduler.Run(t.Context())
	t.Cleanup(func() {
		vs.RecallScheduler.Stop()
		require.NoError(t, vs.Close())
	})

	embeddedCount := func() int64 {
		generations, err := directListGenerations(
			t.Context(), cfg, vector.RecallIndexSpec(),
		)
		if err != nil || len(generations) == 0 {
			return -1
		}
		return generations[0].Embedded
	}
	waitForSchedulerCondition(t, func() bool { return embeddedCount() == 1 },
		"recall backstop never embedded the accepted entry")

	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE recall_entries SET status = 'archived' WHERE id = 'recall-entry'",
		)
		return err
	}))
	waitForSchedulerCondition(t, func() bool { return embeddedCount() == 0 },
		"recall backstop did not remove the archived entry vector")
}

func TestRecallSchedulerSyncRemovesDeletedEntryWithoutExtraction(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSession(t, database, "s1", "agentsview")
	_, err := database.InsertRecallEntry(db.RecallEntry{
		ID: "recall-entry", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Database pool", Body: "Reuse idle connections.",
		SourceSessionID: "s1", ExtractorMethod: "import-v1",
	})
	require.NoError(t, err)

	stub := newEmbeddingsStubServer(t, 3)
	t.Cleanup(stub.Close)
	cfg := vectorTestConfig(dataDir)
	cfg.Vector.Embed.BackstopInterval = "0s"
	embeddingsServer := cfg.Vector.Embeddings.Servers["local"]
	embeddingsServer.Endpoint = stub.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = embeddingsServer

	vs, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(t, err)
	require.NotNil(t, vs.RecallScheduler)
	require.NotNil(t, vs.Close)
	vs.RecallScheduler.debounce = 10 * time.Millisecond
	go vs.RecallScheduler.Run(t.Context())
	t.Cleanup(func() {
		vs.RecallScheduler.Stop()
		require.NoError(t, vs.Close())
	})

	embeddedCount := func() int64 {
		generations, listErr := directListGenerations(
			t.Context(), cfg, vector.RecallIndexSpec(),
		)
		if listErr != nil || len(generations) == 0 {
			return -1
		}
		return generations[0].Embedded
	}
	waitForSchedulerCondition(t, func() bool { return embeddedCount() == 1 },
		"startup reconciliation did not embed the accepted entry")

	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		_, deleteErr := tx.Exec("DELETE FROM recall_entries WHERE id = 'recall-entry'")
		return deleteErr
	}))
	emitter := wrapEmbeddingSyncEmitter(
		&recordingEmitter{}, vs, cfg.Vector.Embed.RunAfterSyncEnabled(),
	)
	emitter.Emit("sync")

	waitForSchedulerCondition(t, func() bool { return embeddedCount() == 0 },
		"successful sync did not remove the deleted Recall vector")
}

func TestRecallSchedulerSessionDeletionRemovesImportedEntryWithoutExtraction(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "permanent delete", path: "/api/v1/sessions/s1/permanent", wantStatus: http.StatusNoContent},
		{name: "empty trash", path: "/api/v1/trash", wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
			dbtest.SeedSession(t, database, "s1", "agentsview")
			_, err := database.InsertRecallEntry(db.RecallEntry{
				ID: "imported-entry", Type: "fact", Scope: "project", Status: "accepted",
				Title: "Imported policy", Body: "Remove this with its source session.",
				SourceSessionID: "s1", ExtractorMethod: "import-v1",
			})
			require.NoError(t, err)
			require.NoError(t, database.SoftDeleteSession("s1"))

			stub := newEmbeddingsStubServer(t, 3)
			t.Cleanup(stub.Close)
			ln, port := listenLoopback(t)
			cfg := vectorTestConfig(dataDir)
			cfg.Host, cfg.Port = "127.0.0.1", port
			cfg.Vector.Embed.BackstopInterval = "0s"
			embeddingsServer := cfg.Vector.Embeddings.Servers["local"]
			embeddingsServer.Endpoint = stub.URL + "/v1"
			cfg.Vector.Embeddings.Servers["local"] = embeddingsServer

			vs, err := setupVectorServing(t.Context(), cfg, database, nil)
			require.NoError(t, err)
			require.NotNil(t, vs.RecallScheduler)
			require.NotNil(t, vs.Close)
			vs.RecallScheduler.debounce = 10 * time.Millisecond
			go vs.RecallScheduler.Run(t.Context())
			t.Cleanup(func() {
				vs.RecallScheduler.Stop()
				require.NoError(t, vs.Close())
			})

			embeddedCount := func() int64 {
				generations, listErr := directListGenerations(
					t.Context(), cfg, vector.RecallIndexSpec(),
				)
				if listErr != nil || len(generations) == 0 {
					return -1
				}
				return generations[0].Embedded
			}
			waitForSchedulerCondition(t, func() bool { return embeddedCount() == 1 },
				"startup reconciliation did not embed the imported entry")

			srv := server.New(cfg, database, nil, vs.ServerOpts...)
			ts := startTestServer(t, ln, srv.Handler())
			req, err := http.NewRequestWithContext(
				t.Context(), http.MethodDelete, ts.URL+tc.path, nil,
			)
			require.NoError(t, err)
			req.Header.Set("Origin", ts.URL)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, tc.wantStatus, resp.StatusCode)

			waitForSchedulerCondition(t, func() bool { return embeddedCount() == 0 },
				"session deletion did not remove the imported Recall vector")
		})
	}
}

func TestRecallSchedulerStartupBuildsOfflineImportedCorpus(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSession(t, database, "s1", "agentsview")
	_, err := database.InsertRecallEntry(db.RecallEntry{
		ID: "offline-import", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Offline policy", Body: "Refresh this entry after daemon restart.",
		SourceSessionID: "s1", ExtractorMethod: "import-v1",
	})
	require.NoError(t, err)

	stub := newEmbeddingsStubServer(t, 3)
	t.Cleanup(stub.Close)
	cfg := vectorTestConfig(dataDir)
	embeddingsServer := cfg.Vector.Embeddings.Servers["local"]
	embeddingsServer.Endpoint = stub.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = embeddingsServer

	vs, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(t, err)
	require.NotNil(t, vs.RecallScheduler)
	require.NotNil(t, vs.Close)
	vs.RecallScheduler.debounce = 10 * time.Millisecond
	go vs.RecallScheduler.Run(t.Context())
	t.Cleanup(func() {
		vs.RecallScheduler.Stop()
		require.NoError(t, vs.Close())
	})

	waitForSchedulerCondition(t, func() bool {
		generations, err := directListGenerations(
			t.Context(), cfg, vector.RecallIndexSpec(),
		)
		return err == nil && len(generations) == 1 && generations[0].Embedded == 1
	}, "daemon startup did not reconcile an offline imported recall entry")
}

func TestRecallSchedulerStartupDoesNotDependOnRunAfterSync(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSession(t, database, "s1", "agentsview")
	_, err := database.InsertRecallEntry(db.RecallEntry{
		ID: "offline-import", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Offline policy", Body: "Refresh Recall independently of session sync.",
		SourceSessionID: "s1", ExtractorMethod: "import-v1",
	})
	require.NoError(t, err)

	stub := newEmbeddingsStubServer(t, 3)
	t.Cleanup(stub.Close)
	cfg := vectorTestConfig(dataDir)
	disabled := false
	cfg.Vector.Embed.RunAfterSync = &disabled
	embeddingsServer := cfg.Vector.Embeddings.Servers["local"]
	embeddingsServer.Endpoint = stub.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = embeddingsServer

	vs, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(t, err)
	require.NotNil(t, vs.RecallScheduler)
	require.NotNil(t, vs.RecallMutationNotify,
		"non-sync Recall mutations must still schedule a refresh")
	require.NotNil(t, vs.Close)
	vs.RecallScheduler.debounce = 10 * time.Millisecond
	go vs.RecallScheduler.Run(t.Context())
	t.Cleanup(func() {
		vs.RecallScheduler.Stop()
		require.NoError(t, vs.Close())
	})

	waitForSchedulerCondition(t, func() bool {
		generations, listErr := directListGenerations(
			t.Context(), cfg, vector.RecallIndexSpec(),
		)
		return listErr == nil && len(generations) == 1 && generations[0].Embedded == 1
	}, "Recall startup reconciliation must not depend on run_after_sync")

	_, err = database.InsertRecallEntry(db.RecallEntry{
		ID: "runtime-import", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Runtime policy", Body: "Refresh this non-sync mutation too.",
		SourceSessionID: "s1", ExtractorMethod: "import-v1",
	})
	require.NoError(t, err)
	vs.RecallMutationNotify()
	waitForSchedulerCondition(t, func() bool {
		generations, listErr := directListGenerations(
			t.Context(), cfg, vector.RecallIndexSpec(),
		)
		return listErr == nil && len(generations) == 1 && generations[0].Embedded == 2
	}, "a non-sync Recall mutation must refresh when run_after_sync is disabled")
}

func TestRecallSearchRejectsCorpusMutationUntilRefresh(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSession(t, database, "s1", "agentsview")
	_, err := database.InsertRecallEntry(db.RecallEntry{
		ID: "entry-1", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Database pool", Body: "Reuse idle connections.",
		SourceSessionID: "s1", ExtractorMethod: "import-v1",
	})
	require.NoError(t, err)

	stub := newEmbeddingsStubServer(t, 3)
	t.Cleanup(stub.Close)
	cfg := vectorTestConfig(dataDir)
	embeddingsServer := cfg.Vector.Embeddings.Servers["local"]
	embeddingsServer.Endpoint = stub.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = embeddingsServer

	ix, err := vector.OpenSpec(
		t.Context(), cfg.Vector.ResolvedDBPath(dataDir),
		vector.RecallIndexSpec(), false, cfg.Vector.Embeddings.MaxInputChars,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	encoders, err := vectorDocumentEncoderSet(cfg.Vector.Embeddings)
	require.NoError(t, err)
	mgr := embeddingManager(ix, database, encoders, cfg, vector.RecallIndexSpec().Name)
	started, err := mgr.TryBuild(t.Context(), vector.BuildRequest{})
	require.NoError(t, err)
	require.True(t, started)

	queryEncoder, err := newVectorQueryEncoder(cfg.Vector.Embeddings, "")
	require.NoError(t, err)
	searcher := recallSearcherAdapter{
		ix: ix, enc: queryEncoder, database: database, cfg: cfg,
	}
	_, _, _, err = searcher.SearchRecall(t.Context(), "connection reuse", 5)
	require.NoError(t, err)

	searcher.enc = func(
		ctx context.Context, texts []string,
	) ([][]float32, error) {
		if updateErr := database.Update(func(tx *sql.Tx) error {
			_, execErr := tx.Exec(`
				UPDATE recall_entries
				SET title = 'Connection policy',
					updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
				WHERE id = 'entry-1'`)
			return execErr
		}); updateErr != nil {
			return nil, updateErr
		}
		return queryEncoder(ctx, texts)
	}
	_, _, _, err = searcher.SearchRecall(t.Context(), "connection reuse", 5)
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrSemanticUnavailable)
	assert.Contains(t, err.Error(), "changed during search")

	searcher.enc = queryEncoder

	_, _, _, err = searcher.SearchRecall(t.Context(), "connection reuse", 5)
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrSemanticUnavailable)
	assert.Contains(t, err.Error(), "embeddings build --store recall")

	started, err = mgr.TryBuild(t.Context(), vector.BuildRequest{})
	require.NoError(t, err)
	require.True(t, started)
	_, _, _, err = searcher.SearchRecall(t.Context(), "connection reuse", 5)
	require.NoError(t, err)
}

func TestRecallSchedulerRequiresExplicitOptInForAutomaticBuilds(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSession(t, database, "s1", "agentsview")
	_, err := database.InsertRecallEntry(db.RecallEntry{
		ID: "offline-import", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Offline policy", Body: "Do not send this entry automatically.",
		SourceSessionID: "s1", ExtractorMethod: "import-v1",
	})
	require.NoError(t, err)

	stub := newEmbeddingsStubServer(t, 3)
	t.Cleanup(stub.Close)
	ln, port := listenLoopback(t)
	cfg := vectorTestConfig(dataDir)
	cfg.Host, cfg.Port = "127.0.0.1", port
	cfg.Vector.Embed.Recall = false
	cfg.Vector.Embed.BackstopInterval = "20ms"
	embeddingsServer := cfg.Vector.Embeddings.Servers["local"]
	embeddingsServer.Endpoint = stub.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = embeddingsServer

	vs, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(t, err)
	require.NotNil(t, vs.RecallScheduler)
	require.NotNil(t, vs.Close)
	vs.RecallScheduler.debounce = 10 * time.Millisecond
	go vs.RecallScheduler.Run(t.Context())
	t.Cleanup(func() {
		vs.RecallScheduler.Stop()
		require.NoError(t, vs.Close())
	})

	assert.Never(t, func() bool {
		generations, listErr := directListGenerations(
			t.Context(), cfg, vector.RecallIndexSpec(),
		)
		return listErr != nil || len(generations) != 0
	}, 100*time.Millisecond, 5*time.Millisecond,
		"Recall content must not be embedded automatically without explicit opt-in")

	srv := server.New(cfg, database, nil, vs.ServerOpts...)
	ts := startTestServer(t, ln, srv.Handler())
	input := strings.NewReader(`
{"candidate_id":"second-import","type":"fact","scope":"repository","title":"Import policy","body":"Keep imports manual too.","project":"agentsview","agent":"codex","session_id":"import-session","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
`)
	req, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost,
		ts.URL+"/api/v1/recall/import?allow_placeholder_sessions=true", input,
	)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var imported db.RecallImportResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&imported))
	assert.Equal(t, 1, imported.Imported)

	assert.Never(t, func() bool {
		generations, listErr := directListGenerations(
			t.Context(), cfg, vector.RecallIndexSpec(),
		)
		return listErr != nil || len(generations) != 0
	}, 100*time.Millisecond, 5*time.Millisecond,
		"manual-only configuration must not build Recall embeddings after an import")
}

func TestRecallImportSchedulesEmbeddingRefresh(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO recall_extract_generations
				(fingerprint, state, model, segmenter, params_json)
			VALUES ('extract-active', 'active', 'extract-model', 'turns-v1', '{}')`)
		return err
	}))

	stub := newEmbeddingsStubServer(t, 3)
	t.Cleanup(stub.Close)
	ln, port := listenLoopback(t)
	cfg := vectorTestConfig(dataDir)
	cfg.Host, cfg.Port = "127.0.0.1", port
	embeddingsServer := cfg.Vector.Embeddings.Servers["local"]
	embeddingsServer.Endpoint = stub.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = embeddingsServer

	vs, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(t, err)
	require.NotNil(t, vs.RecallScheduler)
	require.NotNil(t, vs.Close)
	vs.RecallScheduler.debounce = 10 * time.Millisecond
	go vs.RecallScheduler.Run(t.Context())
	t.Cleanup(func() {
		vs.RecallScheduler.Stop()
		require.NoError(t, vs.Close())
	})

	srv := server.New(cfg, database, nil, vs.ServerOpts...)
	ts := startTestServer(t, ln, srv.Handler())
	input := strings.NewReader(`
{"candidate_id":"imported-entry","type":"fact","scope":"repository","title":"Database pool","body":"Reuse idle connections.","project":"agentsview","agent":"codex","session_id":"import-session","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":0,"ordinal_end":0}}
`)
	req, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost,
		ts.URL+"/api/v1/recall/import?allow_placeholder_sessions=true", input,
	)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.URL)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var imported db.RecallImportResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&imported))
	assert.Equal(t, 1, imported.Imported)

	waitForSchedulerCondition(t, func() bool {
		generations, err := directListGenerations(
			t.Context(), cfg, vector.RecallIndexSpec(),
		)
		return err == nil && len(generations) == 1 && generations[0].Embedded == 1
	}, "a corpus-changing import did not schedule recall embedding")
}

// TestEmbeddingsDaemonClientBuildSucceedsThroughRealMiddleware drives a POST
// build request from embeddingsDaemonClient (the same client `embeddings
// build` uses to talk to a running daemon) through the server's real
// middleware chain (srv.Handler(), not a bare mux), proving the CSRF guard
// in internal/server/server.go's corsMiddleware does not reject it. Every
// other embeddings-daemon-dispatch test in embeddings_test.go serves off an
// httptest.NewServeMux() directly, bypassing that middleware entirely, which
// is exactly how the missing Origin header regression went unnoticed.
func TestEmbeddingsDaemonClientBuildSucceedsThroughRealMiddleware(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	ln, port := listenLoopback(t)
	cfg := vectorTestConfig(dataDir)
	cfg.Host, cfg.Port = "127.0.0.1", port
	vs, err := setupVectorServing(context.Background(), cfg, database, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, vs.Close()) }()

	srv := server.New(cfg, database, nil, vs.ServerOpts...)
	ts := startTestServer(t, ln, srv.Handler())

	client := embeddingsDaemonClient{baseURL: ts.URL}
	err = client.startBuild(context.Background(), vector.BuildRequest{})
	require.NoError(t, err,
		"a POST build must succeed once the client sets Origin to satisfy the CSRF guard")
}

func TestVectorServingCloseWaitsForAPIStartedRecallBuild(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedSession(t, database, "s1", "agentsview")
	_, err := database.InsertRecallEntry(db.RecallEntry{
		ID: "manual-entry", Type: "fact", Scope: "project", Status: "accepted",
		Title: "Manual build", Body: "Keep the index open.", SourceSessionID: "s1",
	})
	require.NoError(t, err)

	encodeStarted := make(chan struct{})
	encodeRelease := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	releaseEncode := func() { releaseOnce.Do(func() { close(encodeRelease) }) }
	defer releaseEncode()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		startedOnce.Do(func() { close(encodeStarted) })
		<-encodeRelease
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{
				"index": i, "embedding": []float32{1, 0, 0},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(stub.Close)

	ln, port := listenLoopback(t)
	cfg := vectorTestConfig(dataDir)
	cfg.Host, cfg.Port = "127.0.0.1", port
	cfg.Vector.Embed.Recall = false
	embeddingsServer := cfg.Vector.Embeddings.Servers["local"]
	embeddingsServer.Endpoint = stub.URL + "/v1"
	cfg.Vector.Embeddings.Servers["local"] = embeddingsServer
	vs, err := setupVectorServing(t.Context(), cfg, database, nil)
	require.NoError(t, err)
	var closeOnce sync.Once
	var vectorCloseErr error
	closeVectorServing := func() error {
		closeOnce.Do(func() { vectorCloseErr = vs.Close() })
		return vectorCloseErr
	}
	t.Cleanup(func() { require.NoError(t, closeVectorServing()) })

	srv := server.New(cfg, database, nil, vs.ServerOpts...)
	ts := startTestServer(t, ln, srv.Handler())
	client := embeddingsDaemonClient{baseURL: ts.URL}
	require.NoError(t, client.startBuild(t.Context(), vector.BuildRequest{
		Store: vector.RecallIndexSpec().Name,
	}))
	select {
	case <-encodeStarted:
	case <-time.After(time.Second):
		require.Fail(t, "manual Recall build never reached the encoder")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- closeVectorServing() }()
	select {
	case closeErr := <-closeDone:
		require.NoError(t, closeErr)
		require.Fail(t, "vector serving closed while the API build was active")
	case <-time.After(100 * time.Millisecond):
	}
	releaseEncode()
	select {
	case closeErr := <-closeDone:
		require.NoError(t, closeErr)
	case <-time.After(2 * time.Second):
		require.Fail(t, "vector serving did not close after the API build completed")
	}
}

// TestSetupVectorServingDisablesWhenWriteLockHeld asserts a held
// vectors.write.lock (simulating a concurrent direct `embeddings build`)
// makes setupVectorServing degrade to a fully-disabled vectorServing after a
// short retry window, rather than blocking or failing daemon startup, and
// that it logs a clear warning explaining why.
func TestSetupVectorServingDisablesWhenWriteLockHeld(t *testing.T) {
	origInterval, origTimeout := vectorsWriteLockRetryInterval, vectorsWriteLockRetryTimeout
	vectorsWriteLockRetryInterval = time.Millisecond
	vectorsWriteLockRetryTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		vectorsWriteLockRetryInterval = origInterval
		vectorsWriteLockRetryTimeout = origTimeout
	})

	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	cfg := vectorTestConfig(dataDir)

	held, err := tryAcquireNamedLock(dataDir, vectorsWriteLockFile)
	require.NoError(t, err)
	defer func() { require.NoError(t, held.Close()) }()

	logBuf := captureLogOutput(t)

	vs, err := setupVectorServing(context.Background(), cfg, database, nil)
	require.NoError(t, err, "a held lock must degrade, not fail, daemon startup")
	assert.Nil(t, vs.Scheduler)
	assert.Nil(t, vs.Close)
	assert.Contains(t, logBuf.String(), "vectors.write.lock")
	assert.Contains(t, logBuf.String(), "disabling vector serving")

	// The degraded run still carries one server option: the unavailability
	// reason, so the embeddings routes' 501s explain the lock conflict and
	// the restart remedy instead of the generic "manager not available"
	// (the reason-to-501 threading itself is pinned in internal/server's
	// TestEmbeddingsUnavailableReasonReplacesGeneric501).
	assert.Len(t, vs.ServerOpts, 1)
}

// TestSetupVectorServingAcquiresAndReleasesWriteLock asserts a free
// vectors.write.lock is acquired for the daemon's lifetime and released by
// Close, so a second setupVectorServing call after Close succeeds rather
// than finding the lock still held.
func TestSetupVectorServingAcquiresAndReleasesWriteLock(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	cfg := vectorTestConfig(dataDir)

	vs, err := setupVectorServing(context.Background(), cfg, database, nil)
	require.NoError(t, err)
	require.NotNil(t, vs.Close)

	// While vs holds the lock, a competing direct acquire must fail.
	_, lockErr := tryAcquireNamedLock(dataDir, vectorsWriteLockFile)
	require.Error(t, lockErr, "setupVectorServing must hold vectors.write.lock while running")

	require.NoError(t, vs.Close())

	// Once released, a fresh acquire — standing in for a second
	// setupVectorServing call — must succeed.
	held, err := tryAcquireNamedLock(dataDir, vectorsWriteLockFile)
	require.NoError(t, err, "Close must release vectors.write.lock")
	require.NoError(t, held.Close())
}

func TestServeConstructionKeepsEmbeddingsRoutesUnavailableWhenVectorDisabled(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	ln, port := listenLoopback(t)
	cfg := config.Config{DataDir: dataDir, DBPath: filepath.Join(dataDir, "sessions.db"),
		Host: "127.0.0.1", Port: port}
	vs, err := setupVectorServing(context.Background(), cfg, database, nil)
	require.NoError(t, err)
	assert.Nil(t, vs.Scheduler)
	assert.Nil(t, vs.Close)
	assert.Empty(t, vs.ServerOpts)

	srv := server.New(cfg, database, nil, vs.ServerOpts...)
	ts := startTestServer(t, ln, srv.Handler())

	resp, err := http.Get(ts.URL + "/api/v1/embeddings/status")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode,
		"the embeddings API should be documented and registered, but unavailable when vector is disabled")
	assert.Equal(t, "application/json", mediaType(t, resp.Header.Get("Content-Type")))
}

// mediaType extracts the bare MIME type from a Content-Type header value,
// dropping any "; charset=..." parameters, so tests can compare it exactly.
func mediaType(t *testing.T, contentType string) string {
	t.Helper()
	mt, _, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)
	return mt
}

// --- installDirectVectorSearcher (direct, non-daemon CLI path) ---

func TestInstallDirectVectorSearcherNoOpWhenVectorDisabled(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	cfg := config.Config{DataDir: dataDir}
	closeFn := installDirectVectorSearcher(cfg, database)
	assert.Nil(t, closeFn)
	assert.False(t, database.HasSemantic())
}

func TestInstallDirectVectorSearcherNoOpWhenVectorsDBMissing(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	cfg := vectorTestConfig(dataDir)
	closeFn := installDirectVectorSearcher(cfg, database)
	assert.Nil(t, closeFn)
	assert.False(t, database.HasSemantic(),
		"no searcher wired means callers see db.ErrSemanticUnavailable naturally")
}

func TestInstallDirectVectorSearcherWiresSearcherWhenVectorsDBExists(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	cfg := vectorTestConfig(dataDir)

	// Create vectors.db up front, as a prior `embeddings build` would.
	seed, err := vector.Open(context.Background(), cfg.Vector.ResolvedDBPath(dataDir), false, 1000)
	require.NoError(t, err)
	require.NoError(t, seed.Close())

	closeFn := installDirectVectorSearcher(cfg, database)
	require.NotNil(t, closeFn)
	defer func() { assert.NoError(t, closeFn()) }()

	assert.True(t, database.HasSemantic(),
		"an existing vectors.db must wire a read-only searcher for direct CLI reads")
}

// TestInstallDirectVectorSearcherDegradesOnCorruptVectorsDB is a regression
// test for a bug where a corrupt or incompatible vectors.db broke direct
// service construction entirely, taking down unrelated commands like
// `session list` that never touch the vector index. A garbage vectors.db
// file must degrade to "no searcher installed" rather than propagate an
// error, leaving non-semantic reads unaffected and semantic search falling
// back to the standard unavailable error.
func TestInstallDirectVectorSearcherDegradesOnCorruptVectorsDB(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	cfg := vectorTestConfig(dataDir)

	// Not a SQLite file at all — simulates corruption or an incompatible
	// build rather than a partially-written one.
	vectorsPath := cfg.Vector.ResolvedDBPath(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(vectorsPath), 0o755))
	require.NoError(t, os.WriteFile(vectorsPath, []byte("not a sqlite database"), 0o644))

	closeFn := installDirectVectorSearcher(cfg, database)
	assert.Nil(t, closeFn,
		"a corrupt vectors.db must not return a handle to close")
	assert.False(t, database.HasSemantic(),
		"a corrupt vectors.db must degrade to no searcher, not fail construction")

	_, err := database.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "query",
		Mode:    "semantic",
		Limit:   5,
	})
	assert.ErrorIs(t, err, db.ErrSemanticUnavailable,
		"semantic search must return the standard unavailable error once degraded")
}

// TestInstallDirectVectorSearcherVersionMismatchServesRebuildRequired pins
// the direct-install path's handling of a version-mismatched vectors.db:
// the read-only open succeeds, so the searcher must be WIRED (not silently
// dropped by the corrupt-file degradation branch) and semantic search over
// HTTP must answer with the 501 rebuild-required taxonomy whose body names
// the incompatible-version remediation — not the generic "not enabled"
// message and not an empty page.
func TestInstallDirectVectorSearcherVersionMismatchServesRebuildRequired(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	cfg := vectorTestConfig(dataDir)
	path := cfg.Vector.ResolvedDBPath(dataDir)

	// Create a current vectors.db, then restamp it as written by the
	// previous mirror schema version.
	seed, err := vector.Open(context.Background(), path, false, cfg.Vector.Embeddings.MaxInputChars)
	require.NoError(t, err)
	require.NoError(t, seed.Close())
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.Exec(`UPDATE vector_meta SET value = '2' WHERE key = 'mirror_schema_version'`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	closeFn := installDirectVectorSearcher(cfg, database)
	require.NotNil(t, closeFn,
		"a version-mismatched vectors.db must wire a searcher, not silently unwire semantic search")
	defer func() { assert.NoError(t, closeFn()) }()
	require.True(t, database.HasSemantic())

	ln, port := listenLoopback(t)
	cfg.Host, cfg.Port = "127.0.0.1", port
	srv := server.New(cfg, database, nil)
	ts := startTestServer(t, ln, srv.Handler())

	req, err := http.NewRequest(http.MethodGet,
		ts.URL+"/api/v1/search/content?pattern=anything&mode=semantic", nil)
	require.NoError(t, err)
	req.Header.Set("X-AgentsView-Search-Intent", "semantic")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotImplemented, resp.StatusCode,
		"body: %s", body)
	assert.Contains(t, string(body), "incompatible version",
		"the error body must carry the rebuild-required remediation")
	assert.Contains(t, string(body), "embeddings build",
		"the error body must tell the user how to rebuild")
}
