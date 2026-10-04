package extract

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// concurrencyWait bounds every wait in these tests. It only fires when the
// behavior under test is broken, so it is generous.
const concurrencyWait = 10 * time.Second

// trackingServer is a model endpoint that records how many distillation
// calls are in flight at once and the order units arrive in.
type trackingServer struct {
	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	texts       []string
}

func (s *trackingServer) enter(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight++
	s.maxInFlight = max(s.maxInFlight, s.inFlight)
	s.texts = append(s.texts, text)
}

func (s *trackingServer) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
}

func (s *trackingServer) peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInFlight
}

func (s *trackingServer) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

// newTrackingServer answers each call through respond, which may block on
// the request context to model a slow endpoint.
func newTrackingServer(
	t *testing.T, respond func(r *http.Request, text string) (int, string),
) (*httptest.Server, *trackingServer) {
	t.Helper()
	tracker := &trackingServer{}
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var payload struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.UnmarshalRead(r.Body, &payload); err != nil {
				assert.Failf(t, "test failed", "decoding request: %v", err)
				return
			}
			text := payload.Messages[len(payload.Messages)-1].Content
			tracker.enter(text)
			defer tracker.leave()
			status, body := respond(r, text)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(server.Close)
	return server, tracker
}

// waitForPeers holds a call until want calls are in flight together, so a
// pass that never runs sessions in parallel is observed as a peak of one
// instead of racing to a lucky overlap. It gives up after concurrencyWait so
// a sequential pass still finishes and fails the peak assertion.
type waitForPeers struct {
	mu      sync.Mutex
	arrived int
	want    int
	ready   chan struct{}
}

func newWaitForPeers(want int) *waitForPeers {
	return &waitForPeers{want: want, ready: make(chan struct{})}
}

func (w *waitForPeers) wait(ctx context.Context) {
	w.mu.Lock()
	w.arrived++
	if w.arrived == w.want {
		close(w.ready)
	}
	w.mu.Unlock()
	select {
	case <-w.ready:
	case <-ctx.Done():
	case <-time.After(concurrencyWait):
	}
}

func TestManagerDistillsSessionsInParallelUpToConcurrency(t *testing.T) {
	d := newTestArchive(t)
	ctx := t.Context()
	peers := newWaitForPeers(2)
	server, tracker := newTrackingServer(t, func(r *http.Request, _ string) (int, string) {
		peers.wait(r.Context())
		return http.StatusOK, completionBody(t, entriesJSON(t, "x"))
	})
	ids := []string{"sess-a", "sess-b", "sess-c", "sess-d", "sess-e"}
	for _, id := range ids {
		seedSession(t, d, id, turnMessages("ask "+id, "answer "+id), nil)
	}
	m := newManager(t, d, server.URL, func(c *ManagerConfig) {
		c.Concurrency = 2
	})

	result, err := m.RunPass(ctx, PassOptions{})
	require.NoError(t, err)
	assert.Equal(t, PassResult{
		Sessions: 5, Units: 10, Entries: 10, Activated: true,
	}, result)
	assert.Equal(t, 2, tracker.peak(),
		"two sessions must distill together and never more than two")

	// Each session is owned by one worker, so its units still reach the
	// model in order: the intent unit before the action unit.
	order := tracker.order()
	for _, id := range ids {
		intent, action := -1, -1
		for i, text := range order {
			switch {
			case strings.Contains(text, "ask "+id):
				intent = i
			case strings.Contains(text, "answer "+id):
				action = i
			}
		}
		require.NotEqual(t, -1, intent, "intent unit of %s", id)
		require.NotEqual(t, -1, action, "action unit of %s", id)
		assert.Less(t, intent, action, "units of %s must distill in order", id)

		progress, found, err := d.ExtractProgress(ctx, id, m.Fingerprint())
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, db.ExtractProgressDone, progress.State)
		assert.Equal(t, progress.UnitsTotal, progress.UnitCursor)
	}
}

func TestManagerDefaultConcurrencyDistillsOneSessionAtATime(t *testing.T) {
	d := newTestArchive(t)
	server, tracker := newTrackingServer(t, func(*http.Request, string) (int, string) {
		return http.StatusOK, completionBody(t, entriesJSON(t, "x"))
	})
	for _, id := range []string{"sess-a", "sess-b", "sess-c"} {
		seedSession(t, d, id, turnMessages("ask "+id, "answer "+id), nil)
	}
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(t.Context(), PassOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.Sessions)
	assert.Equal(t, 1, tracker.peak(),
		"an unset concurrency keeps the sequential pass")
}

// TestManagerParallelFailureMarkStaysSessionScoped pins that a session-scoped
// failure recorded by one worker neither aborts the pass nor touches the
// progress of sessions other workers are committing at the same time.
func TestManagerParallelFailureMarkStaysSessionScoped(t *testing.T) {
	d := newTestArchive(t)
	ctx := t.Context()
	peers := newWaitForPeers(3)
	server, _ := newTrackingServer(t, func(r *http.Request, text string) (int, string) {
		peers.wait(r.Context())
		if strings.Contains(text, "ask sess-b") {
			return http.StatusBadRequest, `{"error":"content refused"}`
		}
		return http.StatusOK, completionBody(t, entriesJSON(t, "x"))
	})
	for _, id := range []string{"sess-a", "sess-b", "sess-c"} {
		seedSession(t, d, id, turnMessages("ask "+id, "answer "+id), nil)
	}
	m := newManager(t, d, server.URL, func(c *ManagerConfig) {
		c.Concurrency = 3
	})

	result, err := m.RunPass(ctx, PassOptions{})
	require.NoError(t, err)
	assert.Equal(t, PassResult{
		Sessions: 2, Failed: 1, Units: 4, Entries: 4, Activated: true,
	}, result)

	failed, found, err := d.ExtractProgress(ctx, "sess-b", m.Fingerprint())
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, db.ExtractProgressFailed, failed.State)
	assert.Equal(t, 0, failed.UnitCursor)
	for _, id := range []string{"sess-a", "sess-c"} {
		progress, found, err := d.ExtractProgress(ctx, id, m.Fingerprint())
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, db.ExtractProgressDone, progress.State, id)
		assert.Equal(t, 2, progress.UnitCursor, id)
	}
}

// TestManagerParallelAbortCancelsInFlightSessions pins that an error which
// aborts a sequential pass aborts a parallel one too: the pass returns that
// error, the sibling session still at the model is cancelled and left
// resumable instead of failed, and no queued session is started.
func TestManagerParallelAbortCancelsInFlightSessions(t *testing.T) {
	d := newTestArchive(t)
	ctx := t.Context()
	slowArrived := make(chan struct{})
	var once sync.Once
	server, _ := newTrackingServer(t, func(r *http.Request, text string) (int, string) {
		if strings.Contains(text, "ask sess-a") {
			once.Do(func() { close(slowArrived) })
			select {
			case <-r.Context().Done():
			case <-time.After(concurrencyWait):
			}
			return http.StatusOK, completionBody(t, entriesJSON(t, "x"))
		}
		select {
		case <-slowArrived:
		case <-time.After(concurrencyWait):
		}
		return http.StatusUnauthorized, `{"error":"bad api key"}`
	})
	// The backlog runs newest ended first, so sess-c is the one left queued.
	for i, id := range []string{"sess-a", "sess-b", "sess-c"} {
		seedSession(t, d, id, turnMessages("ask "+id, "answer "+id),
			endedAgo(time.Duration(i+1)*time.Hour))
	}
	m := newManager(t, d, server.URL, func(c *ManagerConfig) {
		c.Concurrency = 2
	})

	started := time.Now()
	_, err := m.RunPass(ctx, PassOptions{})
	require.Error(t, err)
	require.NotErrorIs(t, err, context.Canceled,
		"the pass must report the endpoint rejection, not its own cancellation: %v", err)
	assert.Less(t, time.Since(started), concurrencyWait,
		"the in-flight sibling must be cancelled, not awaited")

	for _, id := range []string{"sess-a", "sess-b"} {
		progress, found, err := d.ExtractProgress(ctx, id, m.Fingerprint())
		require.NoError(t, err)
		require.True(t, found, id)
		assert.Equal(t, db.ExtractProgressPending, progress.State,
			"%s must stay resumable without burning its backoff", id)
	}
	_, found, err := d.ExtractProgress(ctx, "sess-c", m.Fingerprint())
	require.NoError(t, err)
	assert.False(t, found, "no session may start after the pass aborted")
}

// TestManagerOutageLoserStaysResumable pins the race between workers whose
// retry ladders exhaust against the same outage together: a worker that
// loses the abort claim to a sibling leaves its session resumable instead of
// joining the sibling's session behind the failure backoff.
func TestManagerOutageLoserStaysResumable(t *testing.T) {
	d := newTestArchive(t)
	ctx := t.Context()
	server, _ := newTrackingServer(t, func(*http.Request, string) (int, string) {
		return http.StatusInternalServerError, `{"error":"upstream down"}`
	})
	seedSession(t, d, "sess-a", turnMessages("ask", "answer"), nil)
	m := newManager(t, d, server.URL, func(c *ManagerConfig) {
		c.Concurrency = 2
	})
	require.NoError(t, m.ensureGeneration(ctx))
	// A sibling already claimed the abort, and its cancellation has not
	// reached this worker's context yet.
	abort := &passAbort{ctx: ctx, cancel: func() {}}
	require.True(t, abort.claim(errors.New("sibling exhausted its ladder")))

	outcome, err := m.extractSession(ctx, "sess-a", false, false, abort)
	_, transient := errors.AsType[*transientError](err)
	require.True(t, transient, "the loser still reports the outage: %v", err)
	assert.False(t, outcome.failed)

	progress, found, err := d.ExtractProgress(ctx, "sess-a", m.Fingerprint())
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, db.ExtractProgressPending, progress.State,
		"one outage must back off one session, not every one in flight")
}

// TestManagerOutageMarkFailureBecomesPassCause pins that when the worker
// claiming an outage abort cannot record its session's failure, the pass
// reports that storage error rather than the outage it was acting on.
func TestManagerOutageMarkFailureBecomesPassCause(t *testing.T) {
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	ctx := t.Context()
	var closeOnce sync.Once
	server, _ := newTrackingServer(t, func(*http.Request, string) (int, string) {
		closeOnce.Do(func() { assert.NoError(t, d.Close()) })
		return http.StatusInternalServerError, `{"error":"upstream down"}`
	})
	seedSession(t, d, "sess-a", turnMessages("ask", "answer"), nil)
	m := newManager(t, d, server.URL, nil)
	require.NoError(t, m.ensureGeneration(ctx))
	passCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	abort := &passAbort{ctx: passCtx, cancel: cancel}

	_, err = m.extractSession(passCtx, "sess-a", false, false, abort)
	require.Error(t, err)
	_, transient := errors.AsType[*transientError](err)
	assert.False(t, transient, "the mark failure must surface: %v", err)
	assert.Equal(t, err, abort.error())
}

// TestManagerParallelCancellationLeavesSessionsResumable pins the shutdown
// path: cancelling the pass context while several sessions are at the model
// returns the cancellation and leaves every started row resumable.
func TestManagerParallelCancellationLeavesSessionsResumable(t *testing.T) {
	d := newTestArchive(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	peers := newWaitForPeers(2)
	server, _ := newTrackingServer(t, func(r *http.Request, _ string) (int, string) {
		peers.wait(r.Context())
		cancel()
		<-r.Context().Done()
		return http.StatusOK, completionBody(t, entriesJSON(t, "x"))
	})
	// The backlog runs newest ended first, so sess-c is the one left queued.
	for i, id := range []string{"sess-a", "sess-b", "sess-c"} {
		seedSession(t, d, id, turnMessages("ask "+id, "answer "+id),
			endedAgo(time.Duration(i+1)*time.Hour))
	}
	m := newManager(t, d, server.URL, func(c *ManagerConfig) {
		c.Concurrency = 2
	})

	_, err := m.RunPass(ctx, PassOptions{})
	require.ErrorIs(t, err, context.Canceled)

	check := t.Context()
	for _, id := range []string{"sess-a", "sess-b"} {
		progress, found, err := d.ExtractProgress(check, id, m.Fingerprint())
		require.NoError(t, err)
		require.True(t, found, id)
		assert.Equal(t, db.ExtractProgressPending, progress.State, id)
		assert.Equal(t, 0, progress.UnitCursor, id)
	}
	_, found, err := d.ExtractProgress(check, "sess-c", m.Fingerprint())
	require.NoError(t, err)
	assert.False(t, found, "no session may start after cancellation")
}
