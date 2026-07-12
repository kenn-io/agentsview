package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
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
		})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
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
	f := newIdleTrackerFixture(t, 20*time.Millisecond)

	done := requireBeginWork(t, f.tracker)
	f.run(t)

	f.requireNotFiredWithin(t, 35*time.Millisecond,
		"idle fired while internal work was active")

	done()
	f.requireFiredWithin(t, 80*time.Millisecond,
		"idle did not fire after internal work ended")
}

func TestIdleTrackerTouchResetsIdleBeforeRun(t *testing.T) {
	f := newIdleTrackerFixture(t, 30*time.Millisecond)
	time.Sleep(45 * time.Millisecond)
	f.tracker.Touch()

	f.run(t)

	f.requireNotFiredWithin(t, 15*time.Millisecond,
		"idle fired immediately after touch")
	f.requireFiredWithin(t, 80*time.Millisecond,
		"idle did not fire after touched timeout elapsed")
}

func TestIdleTrackerRejectsRequestsAfterDrainStarts(t *testing.T) {
	f := newIdleTrackerFixture(t, 1*time.Millisecond)
	f.run(t)

	f.requireFiredWithin(t, time.Second, "idle did not fire after timeout")

	rec := serveWrappedNoContent(t, f.tracker)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	assertBeginWorkRejected(t, f.tracker)
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
