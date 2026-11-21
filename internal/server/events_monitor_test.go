package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

func (s *watchStateStore) GetSession(ctx context.Context, id string) (*db.Session, error) {
	state := s.state.Load().(db.SessionWatchState)
	if state.State != db.SessionWatchResolved {
		return nil, &db.SessionIdentityError{State: state.State}
	}
	return s.Store.GetSession(ctx, id)
}

func TestEvents_ReconnectsAfterHostedMonitorCloses(t *testing.T) {
	t.Cleanup(sessionwatch.SetTimingsForTest(25*time.Millisecond, 50*time.Millisecond))
	te := setup(t)
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{ID: "watch-a", Project: "project", Machine: "test", Agent: "claude"}))
	store := &watchStateStore{Store: te.db}
	store.state.Store(db.SessionWatchState{State: db.SessionWatchResolved, PublicID: "watch-a", ContentRevision: "one"})
	cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
	handler := wrapTestHandler(cfg, server.New(cfg, store, nil, server.WithBroadcaster(te.broadcaster)).Handler())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	open := func() (*flushRecorder, <-chan struct{}) {
		w := newFlushRecorder()
		done := make(chan struct{})
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/events?session=watch-a", nil)
		go func() { handler.ServeHTTP(w, req); close(done) }()
		te.waitForSSEEvent(t, w, "session_ready", 5*time.Second)
		return w, done
	}
	first, done := open()
	store.state.Store(db.SessionWatchState{State: db.SessionWatchResolved, PublicID: "watch-a", ContentRevision: "two"})
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("stream did not end after monitor stopped")
	}
	assert.Contains(t, first.BodyString(), "event: session_updated\ndata: watch-a\n")
	second, done := open()
	store.state.Store(db.SessionWatchState{State: db.SessionWatchResolved, PublicID: "watch-a", ContentRevision: "three"})
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("reconnected stream did not receive a later transition")
	}
	assert.Contains(t, second.BodyString(), "event: session_updated\ndata: watch-a\n")
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
	called bool
}

func (s *sessionLookupFailureStore) GetSession(context.Context, string) (*db.Session, error) {
	s.called = true
	return nil, errors.New("session read failed")
}

func TestEvents_SessionLookupError(t *testing.T) {
	te := setup(t)
	store := &sessionLookupFailureStore{Store: te.db}
	cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
	te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
	w := te.get(t, "/api/v1/events?session=missing")
	assertStatus(t, w, http.StatusInternalServerError)
	assert.True(t, store.called)
}
