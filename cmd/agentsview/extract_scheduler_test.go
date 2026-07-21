package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/recall/extract"
	"go.kenn.io/agentsview/internal/server"
)

// fakePassManager records every TryPass call and returns scripted
// (started, err) results in order, repeating the last scripted result once
// the script is exhausted (default: started=true, err=nil).
type fakePassManager struct {
	mu      sync.Mutex
	calls   []extract.PassOptions
	results []fakeTryPassResult
}

type fakeTryPassResult struct {
	started bool
	err     error
}

func (f *fakePassManager) TryPass(
	_ context.Context, opts extract.PassOptions,
) (bool, extract.PassResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, opts)
	idx := len(f.calls) - 1
	if idx < len(f.results) {
		r := f.results[idx]
		return r.started, extract.PassResult{}, r.err
	}
	if len(f.results) > 0 {
		r := f.results[len(f.results)-1]
		return r.started, extract.PassResult{}, r.err
	}
	return true, extract.PassResult{}, nil
}

func (f *fakePassManager) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePassManager) callsSnapshot() []extract.PassOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]extract.PassOptions(nil), f.calls...)
}

func TestExtractSchedulerBurstOfNotifyProducesExactlyOnePass(t *testing.T) {
	mgr := &fakePassManager{}
	s := newExtractScheduler(mgr, 20*time.Millisecond, 0, 0, nil)
	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	for range 5 {
		s.Notify()
	}
	waitForSchedulerCondition(t, func() bool { return mgr.callCount() == 1 },
		"debounced pass never ran")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 1, mgr.callCount(), "burst must coalesce into one pass")
	calls := mgr.callsSnapshot()
	assert.True(t, calls[0].Full,
		"the lifetime's first pass carries the startup full top-up")

	s.Notify()
	waitForSchedulerCondition(t, func() bool { return mgr.callCount() == 2 },
		"second debounced pass never ran")
	calls = mgr.callsSnapshot()
	assert.False(t, calls[1].Full,
		"event-driven passes after the startup pass are incremental")
}

func TestExtractSchedulerBackstopTickRunsFullPass(t *testing.T) {
	mgr := &fakePassManager{}
	s := newExtractScheduler(mgr, time.Hour, 20*time.Millisecond, 0, nil)
	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return mgr.callCount() >= 1 },
		"backstop pass never ran")
	calls := mgr.callsSnapshot()
	assert.True(t, calls[0].Full,
		"backstop passes revisit done sessions for digest top-up")
}

func TestExtractSchedulerDroppedBackstopRetriesOnDebouncedPass(t *testing.T) {
	mgr := &fakePassManager{results: []fakeTryPassResult{
		{started: true},  // startup pass clears the initial full carry
		{started: false}, // backstop tick collides with a running pass
		{started: true},
	}}
	s := newExtractScheduler(mgr, 20*time.Millisecond, 30*time.Millisecond, 0, nil)
	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return mgr.callCount() >= 2 },
		"backstop tick never fired")
	s.Notify()
	waitForSchedulerCondition(t, func() bool { return mgr.callCount() >= 3 },
		"debounced pass never ran")
	calls := mgr.callsSnapshot()
	require.GreaterOrEqual(t, len(calls), 3)
	assert.True(t, calls[2].Full,
		"a dropped backstop must carry into the next debounced pass")
}

func TestExtractSchedulerStopTerminatesRun(t *testing.T) {
	mgr := &fakePassManager{}
	s := newExtractScheduler(mgr, time.Hour, 0, 0, nil)
	go s.Run(context.Background())
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not terminate Run")
	}
}

func TestExtractSchedulerNotifyNeverBlocksWithoutAReader(t *testing.T) {
	s := newExtractScheduler(&fakePassManager{}, time.Hour, 0, 0, nil)
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
		t.Fatal("Notify blocked without a running scheduler")
	}
}

func TestExtractTeeEmitterNotifiesScheduler(t *testing.T) {
	mgr := &fakePassManager{}
	s := newExtractScheduler(mgr, 10*time.Millisecond, 0, 0, nil)
	primary := &recordingEmitter{}
	tee := extractTeeEmitter{primary: primary, scheduler: s}

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	tee.Emit("sessions")
	assert.Equal(t, 1, primary.count(),
		"primary emitter must still receive the event")
	waitForSchedulerCondition(t, func() bool { return mgr.callCount() == 1 },
		"emit must schedule a pass")
}

func TestExtractSchedulerCatchupTicksWhenBackstopDisabled(t *testing.T) {
	// With the backstop disabled, sync-driven passes alone would strand a
	// session that ends and then sees no further sync activity: it only
	// becomes eligible after the quiet period, long after the last debounce
	// fired. The catchup ticker keeps scanning incrementally.
	mgr := &fakePassManager{}
	s := newExtractScheduler(mgr, time.Hour, 0, 20*time.Millisecond, nil)
	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return mgr.callCount() >= 2 },
		"catchup passes never ran")
	calls := mgr.callsSnapshot()
	assert.True(t, calls[0].Full,
		"the first catchup tick consumes the startup full carry")
	for _, call := range calls[1:] {
		assert.False(t, call.Full,
			"catchup passes after the startup carry are incremental")
	}
}

func TestExtractSchedulerBackstopSupersedesCatchup(t *testing.T) {
	mgr := &fakePassManager{}
	s := newExtractScheduler(mgr, time.Hour, 20*time.Millisecond, time.Millisecond, nil)
	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return mgr.callCount() >= 2 },
		"backstop passes never ran")
	for _, call := range mgr.callsSnapshot() {
		assert.True(t, call.Full,
			"an enabled backstop replaces catchup ticks with full passes")
	}
}

// blockingPassManager signals when TryPass begins and blocks it until
// releaseOnce is called, so tests can hold a pass in flight deliberately.
type blockingPassManager struct {
	startedOnce sync.Once
	releasedFn  sync.Once
	started     chan struct{}
	release     chan struct{}
}

func (b *blockingPassManager) TryPass(
	_ context.Context, _ extract.PassOptions,
) (bool, extract.PassResult, error) {
	b.startedOnce.Do(func() { close(b.started) })
	<-b.release
	return true, extract.PassResult{}, nil
}

func (b *blockingPassManager) releaseOnce() {
	b.releasedFn.Do(func() { close(b.release) })
}

// TestExtractSchedulerPassHoldsIdleWorkLease pins that a scheduled pass
// counts as daemon work: a detached daemon's idle reaper firing mid-pass
// cancels the shared context under a long model-backed extraction, so large
// backlogs could never complete.
func TestExtractSchedulerPassHoldsIdleWorkLease(t *testing.T) {
	mgr := &blockingPassManager{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	idled := make(chan struct{})
	tracker := server.NewIdleTracker(50*time.Millisecond, func() { close(idled) })
	s := newExtractScheduler(mgr, 5*time.Millisecond, 0, 0, tracker)
	ctx := t.Context()
	go tracker.Run(ctx)
	go s.Run(ctx)
	defer s.Stop()
	// Registered after s.Stop so it runs first: Stop waits for Run, and Run
	// waits inside the blocked TryPass, so a failure exiting before the
	// explicit release would deadlock the test.
	defer mgr.releaseOnce()

	s.Notify()
	<-mgr.started
	select {
	case <-idled:
		t.Fatal("daemon idled out while an extraction pass was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	mgr.releaseOnce()
	select {
	case <-idled:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon never idled once the pass completed")
	}
}

// TestExtractSchedulerStartsNoPassAfterDraining pins the other half of the
// lease contract: once the idle reaper has begun draining, a queued Notify
// must not start a fresh pass under a daemon that is shutting down.
func TestExtractSchedulerStartsNoPassAfterDraining(t *testing.T) {
	mgr := &fakePassManager{}
	idled := make(chan struct{})
	tracker := server.NewIdleTracker(time.Millisecond, func() { close(idled) })
	s := newExtractScheduler(mgr, 10*time.Millisecond, 0, 0, tracker)
	ctx := t.Context()
	go tracker.Run(ctx)
	select {
	case <-idled:
	case <-time.After(2 * time.Second):
		t.Fatal("tracker never went idle")
	}
	go s.Run(ctx)
	defer s.Stop()

	s.Notify()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, mgr.callCount(),
		"a draining daemon must not start new extraction passes")
}

// TestExtractSchedulerStartupPassSurvivesShortIdleTimeout pins that the
// startup debounce holds a work lease: a daemon whose idle timeout is
// shorter than the debounce would otherwise reap itself before the
// lifetime's first pass, every lifetime, and extraction would never run.
func TestExtractSchedulerStartupPassSurvivesShortIdleTimeout(t *testing.T) {
	mgr := &fakePassManager{}
	idled := make(chan struct{})
	tracker := server.NewIdleTracker(20*time.Millisecond, func() { close(idled) })
	s := newExtractScheduler(mgr, 100*time.Millisecond, 0, 0, tracker)
	ctx := t.Context()
	go tracker.Run(ctx)
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return mgr.callCount() >= 1 },
		"startup pass never ran: the daemon idled out before the "+
			"startup debounce elapsed")
	select {
	case <-idled:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon never idled once the startup pass completed")
	}
}

// TestExtractSchedulerCatchupCarriesFailedStartupFullPass pins that a failed
// startup full pass stays carried when the backstop is disabled: catchup
// ticks otherwise run incremental passes forever, and without another sync
// notification the debounce never re-fires, so completed sessions whose
// transcripts changed between daemon lifetimes would never be revisited.
func TestExtractSchedulerCatchupCarriesFailedStartupFullPass(t *testing.T) {
	mgr := &fakePassManager{results: []fakeTryPassResult{
		{started: true, err: errors.New("model endpoint down")},
		{started: true},
	}}
	s := newExtractScheduler(mgr, 5*time.Millisecond, 0, 40*time.Millisecond, nil)
	go s.Run(t.Context())
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return mgr.callCount() >= 3 },
		"catchup ticks never ran")
	calls := mgr.callsSnapshot()
	require.True(t, calls[0].Full, "the startup pass carries the full top-up")
	assert.True(t, calls[1].Full,
		"a failed startup full pass must carry into the next catchup tick")
	assert.False(t, calls[2].Full,
		"the carry clears once a full pass starts and succeeds")
}

// TestExtractSchedulerRunsStartupPass pins that every daemon lifetime
// begins with one pass, Notify or not: deferred work — a session whose
// quiet period elapsed after the previous daemon exited, retraction for a
// session trashed while no daemon ran — is otherwise only picked up if
// sync activity or a backstop tick happens to arrive before the idle
// timeout ends this lifetime too.
func TestExtractSchedulerRunsStartupPass(t *testing.T) {
	mgr := &fakePassManager{}
	s := newExtractScheduler(mgr, 20*time.Millisecond, 0, 0, nil)
	go s.Run(t.Context())
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return mgr.callCount() >= 1 },
		"startup pass never ran without a Notify")
	calls := mgr.callsSnapshot()
	assert.True(t, calls[0].Full,
		"the startup pass must be full: a detached daemon idles out "+
			"before the backstop interval, so a completed session whose "+
			"transcript grew between daemon lifetimes is only revisited "+
			"here")
}
