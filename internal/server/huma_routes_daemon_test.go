package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/kit/atomicfile"
)

func TestDaemonPersistenceDisablesShutdownAndPreservesSyncIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopped, suspended, published := false, false, false
		tracker := NewIdleTracker(2*time.Second, func() { stopped = true })
		tracker.SetSyncIdle(time.Second, func() { suspended = true })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go tracker.Run(ctx)
		s := New(config.Config{PublicURL: "http://localhost", PublicOrigins: []string{"http://localhost"}}, nil, nil, WithIdleTracker(tracker), WithDaemonPersistence(func() error { published = true; return nil }))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/v1/daemon/persist", nil)
		req.Header.Set("Origin", "http://localhost")
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.True(t, published)
		time.Sleep(5 * time.Second)
		synctest.Wait()
		assert.False(t, stopped)
		assert.True(t, suspended)
	})
}

func TestDaemonPersistencePublicationFailureKeepsIdleExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopped := false
		tracker := NewIdleTracker(time.Second, func() { stopped = true })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go tracker.Run(ctx)
		s := New(config.Config{PublicURL: "http://localhost", PublicOrigins: []string{"http://localhost"}}, nil, nil, WithIdleTracker(tracker), WithDaemonPersistence(func() error { return errors.New("publication failed") }))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/v1/daemon/persist", nil)
		req.Header.Set("Origin", "http://localhost")
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		time.Sleep(2 * time.Second)
		synctest.Wait()
		assert.True(t, stopped)
	})
}

func TestDaemonPersistenceUnavailableWithoutPublisher(t *testing.T) {
	s := New(config.Config{PublicURL: "http://localhost", PublicOrigins: []string{"http://localhost"}}, nil, nil)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/v1/daemon/persist", nil)
	req.Header.Set("Origin", "http://localhost")
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
}

func TestDaemonPersistenceDuringArchiveRebuild(t *testing.T) {
	f := newSyncRouteFixture(t, withStaleDB())
	published := false
	f.srv.persistDaemon = func() error { published = true; return nil }
	require.NoError(t, f.db.CloseWriter())
	res := serveJSON(t, f.handler, http.MethodPost, "/api/v1/daemon/persist", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.True(t, published)
}

func TestDaemonPersistencePublishedWriteFailureKeepsDaemonRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopped := false
		tracker := NewIdleTracker(time.Second, func() { stopped = true })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go tracker.Run(ctx)
		publicationErr := errors.Join(atomicfile.ErrNotDurable, errors.New("directory fsync failed"))
		s := New(config.Config{PublicURL: "http://localhost", PublicOrigins: []string{"http://localhost"}}, nil, nil, WithIdleTracker(tracker), WithDaemonPersistence(func() error { return publicationErr }))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://localhost/api/v1/daemon/persist", nil)
		req.Header.Set("Origin", "http://localhost")
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "directory fsync failed")
		time.Sleep(5 * time.Second)
		synctest.Wait()
		assert.False(t, stopped, "the visible persistent lifetime must match live shutdown behavior")
	})
}
