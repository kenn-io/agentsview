package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/recall/extract"
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
	s := newExtractScheduler(mgr, 20*time.Millisecond, 0)
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
	assert.False(t, calls[0].Full, "event-driven passes are incremental")
}

func TestExtractSchedulerBackstopTickRunsFullPass(t *testing.T) {
	mgr := &fakePassManager{}
	s := newExtractScheduler(mgr, time.Hour, 20*time.Millisecond)
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
		{started: false}, // backstop tick collides with a running pass
		{started: true},
	}}
	s := newExtractScheduler(mgr, 20*time.Millisecond, 30*time.Millisecond)
	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return mgr.callCount() >= 1 },
		"backstop tick never fired")
	s.Notify()
	waitForSchedulerCondition(t, func() bool { return mgr.callCount() >= 2 },
		"debounced pass never ran")
	calls := mgr.callsSnapshot()
	require.GreaterOrEqual(t, len(calls), 2)
	assert.True(t, calls[1].Full,
		"a dropped backstop must carry into the next debounced pass")
}

func TestExtractSchedulerStopTerminatesRun(t *testing.T) {
	mgr := &fakePassManager{}
	s := newExtractScheduler(mgr, time.Hour, 0)
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
	s := newExtractScheduler(&fakePassManager{}, time.Hour, 0)
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
	s := newExtractScheduler(mgr, 10*time.Millisecond, 0)
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
