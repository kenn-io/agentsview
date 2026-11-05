package main

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/recall/extract"
	"go.kenn.io/agentsview/internal/server"
	syncengine "go.kenn.io/agentsview/internal/sync"
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
	result  extract.PassResult
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
		return r.started, r.result, r.err
	}
	if len(f.results) > 0 {
		r := f.results[len(f.results)-1]
		return r.started, r.result, r.err
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
	synctest.Test(t, func(t *testing.T) {
		mgr := &fakePassManager{}
		s := newExtractScheduler(mgr, 20*time.Millisecond, 0, 0, nil)
		go s.Run(t.Context())
		defer s.Stop()

		for range 5 {
			s.Notify()
		}
		synctest.Sleep(50 * time.Millisecond)
		require.Equal(t, 1, mgr.callCount(), "debounced pass never ran")
		calls := mgr.callsSnapshot()
		assert.True(t, calls[0].Full,
			"the lifetime's first pass carries the startup full top-up")

		s.Notify()
		synctest.Sleep(50 * time.Millisecond)
		calls = mgr.callsSnapshot()
		require.Equal(t, 2, mgr.callCount(), "second debounced pass never ran")
		assert.False(t, calls[1].Full,
			"event-driven passes after the startup pass are incremental")
	})
}

func TestExtractSchedulerNotifiesDownstreamAfterEveryStartedPass(t *testing.T) {
	mgr := &fakePassManager{results: []fakeTryPassResult{
		{started: false},
		{
			started: true,
			result:  extract.PassResult{Sessions: 1, Entries: 2},
			err:     errors.New("later extraction failed"),
		},
		{started: true},
	}}
	s := newExtractScheduler(mgr, time.Hour, 0, 0, nil)
	var notified int
	s.onPassFinished = func() { notified++ }

	started, ok, err := s.tryPassWithLease(t.Context(), mgr, extract.PassOptions{})
	assert.False(t, started)
	assert.True(t, ok)
	require.NoError(t, err)
	assert.Zero(t, notified)

	started, ok, err = s.tryPassWithLease(t.Context(), mgr, extract.PassOptions{})
	assert.True(t, started)
	assert.True(t, ok)
	require.EqualError(t, err, "later extraction failed")
	assert.Equal(t, 1, notified,
		"partial commits must refresh downstream indexes despite a later error")

	started, ok, err = s.tryPassWithLease(t.Context(), mgr, extract.PassOptions{})
	assert.True(t, started)
	assert.True(t, ok)
	require.NoError(t, err)
	assert.Equal(t, 2, notified)
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
	synctest.Test(t, func(t *testing.T) {
		mgr := &fakePassManager{}
		s := newExtractScheduler(mgr, time.Hour, 0, 0, nil)
		go s.Run(t.Context())
		done := make(chan struct{})
		go func() {
			s.Stop()
			close(done)
		}()
		synctest.Wait()
		select {
		case <-done:
		default:
			require.FailNow(t, "Stop did not terminate Run")
		}
	})
}

func TestExtractSchedulerNotifyNeverBlocksWithoutAReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newExtractScheduler(&fakePassManager{}, time.Hour, 0, 0, nil)
		done := make(chan struct{})
		go func() {
			for range 100 {
				s.Notify()
			}
			close(done)
		}()
		synctest.Wait()
		select {
		case <-done:
		default:
			require.FailNow(t, "Notify blocked without a running scheduler")
		}
	})
}

func TestExtractTeeEmitterNotifiesScheduler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := &fakePassManager{}
		s := newExtractScheduler(mgr, 10*time.Millisecond, 0, 0, nil)
		primary := &recordingEmitter{}
		tee := extractTeeEmitter{primary: primary, scheduler: s}

		go s.Run(t.Context())
		defer s.Stop()

		tee.Emit("sessions")
		assert.Equal(t, 1, primary.count(),
			"primary emitter must still receive the event")
		synctest.Sleep(10 * time.Millisecond)
		require.Equal(t, 1, mgr.callCount(), "emit must schedule a pass")
	})
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

// blockingPassManager completes the startup pass, then signals and blocks
// later passes until releaseOnce is called.
type blockingPassManager struct {
	passes      int
	startedOnce sync.Once
	releasedFn  sync.Once
	started     chan struct{}
	release     chan struct{}
}

func (b *blockingPassManager) TryPass(
	_ context.Context, _ extract.PassOptions,
) (bool, extract.PassResult, error) {
	b.passes++
	if b.passes == 1 {
		return true, extract.PassResult{}, nil
	}
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
	synctest.Test(t, func(t *testing.T) {
		mgr := &blockingPassManager{
			started: make(chan struct{}),
			release: make(chan struct{}),
		}
		idled := make(chan struct{})
		tracker := server.NewIdleTracker(50*time.Millisecond, func() { close(idled) })
		s := newExtractScheduler(mgr, 5*time.Millisecond, 0, 0, tracker)
		finished := make(chan struct{}, 2)
		s.onPassFinished = func() { finished <- struct{}{} }
		ctx := t.Context()
		go tracker.Run(ctx)
		go s.Run(ctx)
		defer s.Stop()
		// Registered after s.Stop so it runs first: Stop waits for Run, and Run
		// waits inside the blocked TryPass, so a failure exiting before the
		// explicit release would deadlock the test.
		defer mgr.releaseOnce()

		<-finished
		synctest.Wait() // Let Run release its startup lease before the next notification.
		s.Notify()
		<-mgr.started
		select {
		case <-idled:
			require.FailNow(t, "daemon idled out while an extraction pass was in flight")
		case <-time.After(200 * time.Millisecond):
		}
		mgr.releaseOnce()
		select {
		case <-idled:
		case <-time.After(2 * time.Second):
			require.FailNow(t, "daemon never idled once the pass completed")
		}
	})
}

// TestExtractSchedulerStartsNoPassAfterDraining pins the other half of the
// lease contract: once the idle reaper has begun draining, a queued Notify
// must not start a fresh pass under a daemon that is shutting down.
func TestExtractSchedulerStartsNoPassAfterDraining(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := &fakePassManager{}
		idled := make(chan struct{})
		tracker := server.NewIdleTracker(time.Millisecond, func() { close(idled) })
		s := newExtractScheduler(mgr, 10*time.Millisecond, 0, 0, tracker)
		go tracker.Run(t.Context())
		synctest.Sleep(time.Millisecond)
		select {
		case <-idled:
		default:
			require.FailNow(t, "tracker never went idle")
		}
		go s.Run(t.Context())
		defer s.Stop()

		s.Notify()
		synctest.Sleep(100 * time.Millisecond)
		assert.Equal(t, 0, mgr.callCount(),
			"a draining daemon must not start new extraction passes")
	})
}

// TestExtractSchedulerStartupPassSurvivesShortIdleTimeout pins that the
// startup debounce holds a work lease: a daemon whose idle timeout is
// shorter than the debounce would otherwise reap itself before the
// lifetime's first pass, every lifetime, and extraction would never run.
func TestExtractSchedulerStartupPassSurvivesShortIdleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
			require.FailNow(t, "daemon never idled once the startup pass completed")
		}
	})
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

func TestExtractSchedulerReconcilesOnBatteryAndCarriesFullPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr, reconciler := &fakePassManager{}, &fakePassManager{}
		var battery atomic.Bool
		battery.Store(true)
		s := newExtractScheduler(mgr, time.Second, 0, 5*time.Minute, nil)
		s.onBattery, s.onBatteryMgr = battery.Load, reconciler
		var notified atomic.Int32
		s.onPassFinished = func() { notified.Add(1) }
		go s.Run(t.Context())
		defer s.Stop()
		synctest.Sleep(time.Second + 2*batteryRecheckInterval)
		synctest.Wait()
		assert.Zero(t, mgr.callCount())
		assert.Equal(t, []extract.PassOptions{{Full: true}}, reconciler.callsSnapshot())
		assert.EqualValues(t, 1, notified.Load())
		for range 5 {
			s.Notify()
		}
		synctest.Sleep(time.Second)
		synctest.Wait()
		assert.Zero(t, mgr.callCount())
		assert.Equal(t, []extract.PassOptions{{Full: true}, {Full: true}}, reconciler.callsSnapshot())
		assert.EqualValues(t, 2, notified.Load())
		synctest.Sleep(3 * time.Minute)
		synctest.Wait()
		assert.Equal(t, []extract.PassOptions{{Full: true}, {Full: true}, {Full: true}}, reconciler.callsSnapshot())
		assert.Zero(t, mgr.callCount())
		assert.EqualValues(t, 3, notified.Load())
	})
}

func TestExtractSchedulerBatteryShortBackstopReleasesStartupLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		idled := make(chan struct{})
		tracker := server.NewIdleTracker(time.Minute, func() { close(idled) })
		go tracker.Run(ctx)
		mgr, reconciler := &fakePassManager{}, &fakePassManager{}
		s := newExtractScheduler(mgr, 30*time.Second, 10*time.Second, 0, tracker)
		s.onBattery, s.onBatteryMgr = func() bool { return true }, reconciler
		go s.Run(ctx)
		defer s.Stop()
		synctest.Sleep(2 * time.Minute)
		synctest.Wait()
		calls := reconciler.callsSnapshot()
		assert.Zero(t, mgr.callCount())
		require.NotEmpty(t, calls)
		for _, opts := range calls {
			assert.Equal(t, extract.PassOptions{Full: true}, opts)
		}
		select {
		case <-idled:
		default:
			assert.Fail(t, "battery reconciliation kept the startup lease")
		}
	})
}

func TestExtractSchedulerBatteryResumesWithoutSync(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr, reconciler := &fakePassManager{}, &fakePassManager{}
		var battery atomic.Bool
		battery.Store(true)
		s := newExtractScheduler(mgr, 30*time.Second, time.Hour, 0, nil)
		s.onBattery, s.onBatteryMgr = battery.Load, reconciler
		go s.Run(t.Context())
		defer s.Stop()
		s.Notify()
		synctest.Sleep(31 * time.Second)
		synctest.Wait()
		require.Zero(t, mgr.callCount())
		require.Equal(t, 1, reconciler.callCount())
		battery.Store(false)
		synctest.Sleep(2 * time.Minute)
		synctest.Wait()
		assert.Equal(t, []extract.PassOptions{{Full: true}}, mgr.callsSnapshot(),
			"AC must resume deferred extraction without another sync or hourly tick")
	})
}

func TestExtractSchedulerPeriodicACPassConsumesBatteryRecheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr, reconciler := &fakePassManager{}, &fakePassManager{}
		var battery atomic.Bool
		battery.Store(true)
		s := newExtractScheduler(mgr, 30*time.Second, 2*time.Minute, 0, nil)
		s.onBattery, s.onBatteryMgr = battery.Load, reconciler
		var notified atomic.Int32
		s.onPassFinished = func() { notified.Add(1) }
		go s.Run(t.Context())
		defer s.Stop()
		synctest.Sleep(100 * time.Second)
		synctest.Wait()
		require.Zero(t, mgr.callCount())
		require.Equal(t, 1, reconciler.callCount())
		battery.Store(false)
		synctest.Sleep(21 * time.Second)
		synctest.Wait()
		require.Equal(t, []extract.PassOptions{{Full: true}}, mgr.callsSnapshot())
		synctest.Sleep(time.Minute)
		synctest.Wait()
		assert.Equal(t, []extract.PassOptions{{Full: true}}, mgr.callsSnapshot(),
			"a completed periodic pass must consume its power retry")
		assert.EqualValues(t, 2, notified.Load(), "completed work must notify Recall only once")
	})
}

func TestExtractSchedulerPeriodicPassPreservesSyncDebounce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mgr := &fakePassManager{}
		s := newExtractScheduler(mgr, 30*time.Second, 2*time.Minute, 0, nil)
		go s.Run(t.Context())
		defer s.Stop()
		synctest.Sleep(100 * time.Second)
		synctest.Wait()
		s.Notify()
		synctest.Wait()
		synctest.Sleep(31 * time.Second)
		synctest.Wait()
		assert.Equal(t, []extract.PassOptions{{Full: true}, {Full: true}, {}}, mgr.callsSnapshot(),
			"the later sync debounce must still scan sessions that become eligible after the periodic pass")
	})
}

func TestReconcilerRetractsAfterLateSignalBackfill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		database := dbtest.OpenTestDB(t)
		dbtest.SeedSessionWithMessages(t, database, "session", "project", []db.Message{
			dbtest.UserMsg("session", 0, "aws "+"AKIA"+"7QHWN2DKR4FYPLJM"),
		})
		_, err := database.EnsureExtractGeneration(t.Context(), db.ExtractGeneration{
			Fingerprint: "generation", Model: "model", Segmenter: "turns-v1",
		})
		require.NoError(t, err)
		_, err = database.InsertExtractedRecallEntries(t.Context(), []db.RecallEntry{{
			ID: "entry", Type: "fact", ReviewState: "unreviewed_auto", Status: "accepted",
			Title: "Stored fact", Body: "Generated before signals were backfilled.",
			SourceSessionID: "session", SourceRunID: "generation", ProvenanceOK: true,
		}})
		require.NoError(t, err)
		reconciler := extract.NewReconciler(database)
		require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET local_modified_at = ? WHERE id = ?", "1999-01-01T00:00:00.000Z", "session")
			return err
		}))
		started, _, err := reconciler.TryPass(t.Context(), extract.PassOptions{})
		require.NoError(t, err)
		require.True(t, started)
		entry, err := database.GetRecallEntry(t.Context(), "entry")
		require.NoError(t, err)
		require.NotNil(t, entry)

		engine := syncengine.NewEngine(t.Context(), database, syncengine.EngineConfig{})
		defer engine.Close()
		require.NoError(t, database.BackfillSignals(t.Context(), engine.BackfillSignalComputer()))
		session, err := database.GetSession(t.Context(), "session")
		require.NoError(t, err)
		require.NotNil(t, session)
		require.Positive(t, session.SecretLeakCount)
		started, _, err = reconciler.TryPass(t.Context(), extract.PassOptions{})
		require.NoError(t, err)
		require.True(t, started)
		entry, err = database.GetRecallEntry(t.Context(), "entry")
		require.NoError(t, err)
		assert.Nil(t, entry, "reconciliation must discover signals backfilled after its previous pass")
	})
}
