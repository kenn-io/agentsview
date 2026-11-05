package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
)

// idleTrackerFixture bundles an IdleTracker with a buffered channel
// that receives a value whenever the tracker reports going idle.
type idleTrackerFixture struct {
	tracker *IdleTracker
	fired   chan time.Time
}

// newIdleTrackerFixture builds a tracker whose onIdle callback signals
// the fixture's fired channel.
func newIdleTrackerFixture(
	t *testing.T, timeout time.Duration,
) *idleTrackerFixture {
	t.Helper()
	fired := make(chan time.Time, 1)
	return &idleTrackerFixture{
		tracker: NewIdleTracker(timeout, func() { fired <- time.Now() }),
		fired:   fired,
	}
}

// run starts the tracker's loop in the background using the test
// context, which is cancelled during test cleanup.
func (f *idleTrackerFixture) run(t *testing.T) {
	t.Helper()
	go f.tracker.Run(t.Context())
}

// requireNotFiredWithin fails if the tracker reports idle within d.
func (f *idleTrackerFixture) requireNotFiredWithin(
	t *testing.T, d time.Duration, msg string,
) {
	t.Helper()
	select {
	case <-f.fired:
		require.FailNow(t, msg)
	case <-time.After(d):
	}
}

// requireFiredWithin fails if the tracker does not report idle within d.
func (f *idleTrackerFixture) requireFiredWithin(
	t *testing.T, d time.Duration, msg string,
) time.Time {
	t.Helper()
	select {
	case firedAt := <-f.fired:
		return firedAt
	case <-time.After(d):
		stacktest.Dump(t)
		require.FailNow(t, msg)
		return time.Time{}
	}
}

// serveWrappedNoContent drives a no-content handler through the
// tracker's Wrap middleware and returns the recorder.
func serveWrappedNoContent(
	t *testing.T, tracker *IdleTracker,
) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	tracker.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), "").ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	return rec
}

// requireBeginWork asserts that BeginWork is accepted and returns its
// done callback.
func requireBeginWork(t *testing.T, tracker *IdleTracker) func() {
	t.Helper()
	done, ok := tracker.BeginWork()
	require.True(t, ok)
	require.NotNil(t, done)
	return done
}

// assertBeginWorkRejected asserts that BeginWork is refused and runs
// the returned done callback.
func assertBeginWorkRejected(t *testing.T, tracker *IdleTracker) {
	t.Helper()
	done, ok := tracker.BeginWork()
	assert.False(t, ok)
	done()
}

func TestIdleTrackerExternalRequestResetsIdle(t *testing.T) {
	timeout := 40 * time.Millisecond
	f := newIdleTrackerFixture(t, timeout)
	entered := make(chan struct{})
	release := make(chan struct{})
	requestDone := make(chan *httptest.ResponseRecorder, 1)

	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRequest)

	go func() {
		rec := httptest.NewRecorder()
		f.tracker.Wrap(http.HandlerFunc(func(
			w http.ResponseWriter, _ *http.Request,
		) {
			close(entered)
			<-release
			w.WriteHeader(http.StatusNoContent)
		}), "").ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
		requestDone <- rec
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		require.FailNow(t, "wrapped request did not enter handler")
	}

	f.run(t)
	f.tracker.mu.Lock()
	lastExternalBeforeRelease := f.tracker.lastExternal
	f.tracker.mu.Unlock()

	f.requireNotFiredWithin(t, 2*timeout,
		"idle fired while external request was active")

	releasedAt := time.Now()
	releaseRequest()

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-requestDone:
	case <-time.After(time.Second):
		require.FailNow(t, "wrapped request did not complete after release")
	}
	assert.Equal(t, http.StatusNoContent, rec.Code)

	f.tracker.mu.Lock()
	lastExternalAfterRelease := f.tracker.lastExternal
	f.tracker.mu.Unlock()
	assert.True(t, lastExternalAfterRelease.After(lastExternalBeforeRelease),
		"request completion did not advance external activity timestamp")

	firedAt := f.requireFiredWithin(t, time.Second,
		"idle did not fire after external activity became idle")
	assert.GreaterOrEqual(t, firedAt.Sub(releasedAt), timeout)
}

func TestIdleTrackerInternalWorkBlocksWithoutResettingIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newIdleTrackerFixture(t, 20*time.Millisecond)

		done := requireBeginWork(t, f.tracker)
		f.run(t)

		f.requireNotFiredWithin(t, 35*time.Millisecond,
			"idle fired while internal work was active")

		done()
		f.requireFiredWithin(t, 80*time.Millisecond,
			"idle did not fire after internal work ended")
	})
}

func TestIdleTrackerTouchResetsIdleBeforeRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newIdleTrackerFixture(t, 30*time.Millisecond)
		f.tracker.lastExternal = time.Now().Add(-45 * time.Millisecond)
		f.tracker.Touch()

		f.run(t)

		f.requireNotFiredWithin(t, 15*time.Millisecond,
			"idle fired immediately after touch")
		f.requireFiredWithin(t, 80*time.Millisecond,
			"idle did not fire after touched timeout elapsed")
	})
}

func TestIdleTrackerRejectsRequestsAfterDrainStarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newIdleTrackerFixture(t, 1*time.Millisecond)
		f.run(t)

		f.requireFiredWithin(t, time.Second, "idle did not fire after timeout")

		rec := serveWrappedNoContent(t, f.tracker)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

		assertBeginWorkRejected(t, f.tracker)
	})
}

func TestIdleTrackerNilReceiverIsNoop(t *testing.T) {
	var tracker *IdleTracker

	done := requireBeginWork(t, tracker)
	done()

	tracker.Touch()
	tracker.Run(t.Context())

	rec := serveWrappedNoContent(t, tracker)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestIdleTrackerSyncSuspensionTracksExternalRequestsAndOutsideWork(t *testing.T) {
	for _, name := range []string{"external request", "outside work"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				tracker := NewIdleTracker(0, func() {})
				suspended := make(chan struct{}, 4)
				tracker.SetSyncIdle(time.Minute, func() { suspended <- struct{}{} })
				switch name {
				case "outside work":
					finish := requireBeginWork(t, tracker)
					defer finish()
					go tracker.Run(ctx)
					time.Sleep(2 * time.Minute)
					synctest.Wait()
					assert.Len(t, suspended, 1, "outside work permits local suspension")
				case "external request":
					go tracker.Run(ctx)
					<-suspended
					entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
					go func() {
						defer close(done)
						tracker.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release }), "").ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/events", nil))
					}()
					<-entered
					time.Sleep(2 * time.Minute)
					assert.Empty(t, suspended, "an in-flight request keeps local sync active")
					close(release)
					<-done
					time.Sleep(59 * time.Second)
					synctest.Wait()
					assert.Empty(t, suspended, "request completion starts a full quiet minute")
					time.Sleep(time.Second)
					<-suspended
				}
			})
		})
	}
}

func TestIdleSyncIgnoresBasePathPing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		tracker := NewIdleTracker(time.Minute, func() { assert.Fail(t, "desktop probes must keep the daemon alive") })
		suspensions := 0
		tracker.SetSyncIdle(time.Minute, func() { suspensions++ })
		handler := tracker.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), "/archive")
		go tracker.Run(ctx)
		for range 6 {
			time.Sleep(30 * time.Second)
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8080/archive/api/ping", nil))
		}
		synctest.Wait()
		require.Equal(t, 1, suspensions)
	})
}
