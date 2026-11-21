package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/sessionwatch"
)

type watchStateStore struct {
	db.Store
	state atomic.Value
}

func (s *watchStateStore) GetSessionWatchState(string) (db.SessionWatchState, error) {
	return s.state.Load().(db.SessionWatchState), nil
}

func TestEvents_EndsWhenHostedMonitorCloses(t *testing.T) {
	t.Cleanup(sessionwatch.SetTimingsForTest(25*time.Millisecond, 50*time.Millisecond))
	te := setup(t)
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{ID: "watch-a", Project: "project", Machine: "test", Agent: "claude"}))
	store := &watchStateStore{Store: te.db}
	store.state.Store(db.SessionWatchState{State: db.SessionWatchResolved, PublicID: "watch-a", ContentRevision: "one"})
	cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
	handler := wrapTestHandler(cfg, server.New(cfg, store, nil, server.WithBroadcaster(te.broadcaster)).Handler())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	w := newFlushRecorder()
	done := make(chan struct{})
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/events?session=watch-a", nil)
	go func() { handler.ServeHTTP(w, req); close(done) }()
	te.waitForSSEEvent(t, w, "session_ready", 5*time.Second)
	store.state.Store(db.SessionWatchState{State: db.SessionWatchResolved, PublicID: "watch-a", ContentRevision: "two"})
	select {
	case <-done:
	case <-ctx.Done():
		require.FailNow(t, "stream did not end after monitor stopped")
	}
	assert.Contains(t, w.BodyString(), "event: session_updated\ndata: watch-a\n")
}

func TestEvents_UnresolvedHostedSessionIsMissing(t *testing.T) {
	te := setup(t)
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{ID: "watch-a", Project: "project", Machine: "test", Agent: "claude"}))
	store := &watchStateStore{Store: te.db}
	store.state.Store(db.SessionWatchState{State: "ambiguous", Variants: []string{"watch-a~one", "watch-a~two"}})
	cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
	handler := wrapTestHandler(cfg, server.New(cfg, store, nil, server.WithBroadcaster(te.broadcaster)).Handler())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	w := newFlushRecorder()
	done := make(chan struct{})
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/events?session=watch-a", nil)
	go func() { handler.ServeHTTP(w, req); close(done) }()
	te.waitForSSEEvent(t, w, "events_ready", 5*time.Second)
	assert.Contains(t, w.BodyString(), "event: session_missing\ndata: watch-a\n")
	assert.NotContains(t, w.BodyString(), "event: session_ready\n")
	cancel()
	<-done
}

type sessionLookupFailureStore struct {
	db.Store
	cancel context.CancelFunc
	called *atomic.Bool
}

func (s *sessionLookupFailureStore) GetSession(ctx context.Context, id string) (*db.Session, error) {
	if id == "bad" && !s.called.Swap(true) {
		if s.cancel != nil {
			s.cancel()
		}
		return nil, errors.New("session read failed")
	}
	return s.Store.GetSession(ctx, id)
}

type watchStateFailureStore struct {
	db.Store
	cancel context.CancelFunc
	called *atomic.Bool
	reads  atomic.Int32
}

func (s *watchStateFailureStore) GetSessionWatchState(id string) (db.SessionWatchState, error) {
	if id == "bad" && s.reads.Add(1) == 1 {
		s.called.Store(true)
		if s.cancel != nil {
			s.cancel()
		}
		return db.SessionWatchState{}, errors.New("watch state read failed")
	}
	return db.SessionWatchState{State: db.SessionWatchResolved, PublicID: id}, nil
}

func TestEvents_SessionLookupError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(db.Store, context.CancelFunc, *atomic.Bool) db.Store
	}{
		{"session", func(store db.Store, cancel context.CancelFunc, called *atomic.Bool) db.Store {
			return &sessionLookupFailureStore{Store: store, cancel: cancel, called: called}
		}},
		{"watch state", func(store db.Store, cancel context.CancelFunc, called *atomic.Bool) db.Store {
			return &watchStateFailureStore{Store: store, cancel: cancel, called: called}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(sessionwatch.SetTimingsForTest(25*time.Millisecond, 50*time.Millisecond))
			te := setup(t)
			var called atomic.Bool
			require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{ID: "watch-a", Project: "project", Machine: "test", Agent: "claude"}))
			require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{ID: "bad", Project: "project", Machine: "test", Agent: "claude"}))
			cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
			handler := wrapTestHandler(cfg, server.New(cfg, tc.store(te.db, nil, &called), nil, server.WithBroadcaster(te.broadcaster)).Handler())
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			w := newFlushRecorder()
			done := make(chan struct{})
			t.Cleanup(func() { cancel(); <-done })
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/events?session=bad&session=watch-a", nil)
			go func() { handler.ServeHTTP(w, req); close(done) }()
			te.waitForSSEEvent(t, w, "events_ready", 5*time.Second)
			assert.True(t, called.Load())
			assert.Contains(t, w.BodyString(), "event: session_ready\ndata: bad\n")
			assert.NotContains(t, w.BodyString(), "event: session_missing\ndata: bad\n")
			assert.Contains(t, w.BodyString(), "event: session_ready\ndata: watch-a\n")
			if tc.name == "session" {
				require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{ID: "bad", Project: "project", Machine: "test", Agent: "claude", MessageCount: 1}))
				require.Eventually(t, func() bool {
					return strings.Count(w.BodyString(), "event: session_updated\ndata: bad\n") >= 1
				}, 5*time.Second, 10*time.Millisecond)
			}
			te.broadcaster.Emit("messages")
			te.waitForSSEEvent(t, w, "data_changed", 5*time.Second)
			assert.Contains(t, w.BodyString(), "event: data_changed\ndata: {\"scope\":\"messages\"}\n")
			cancel()
			<-done
			assert.Equal(t, http.StatusOK, w.Code)
		})
		t.Run(tc.name+" cancelled", func(t *testing.T) {
			te := setup(t)
			var called atomic.Bool
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
			handler := wrapTestHandler(cfg, server.New(cfg, tc.store(te.db, cancel, &called), nil, server.WithBroadcaster(te.broadcaster)).Handler())
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/events?session=bad", nil))
			assertStatus(t, w, http.StatusInternalServerError)
		})
	}
}
