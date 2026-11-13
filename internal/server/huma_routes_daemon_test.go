package server

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestDaemonRestart(t *testing.T) {
	f := newSyncRouteFixture(t, withStaleDB())
	settings := func() bool {
		t.Helper()
		res := serveJSON(t, f.handler, http.MethodGet, "/api/v1/settings", nil)
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		var body struct {
			RestartSupported bool `json:"restart_supported"`
		}
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
		return body.RestartSupported
	}
	assert.False(t, settings())
	res := serveJSON(t, f.handler, http.MethodPost, "/api/v1/daemon/restart", nil)
	require.Equal(t, http.StatusNotImplemented, res.Code, res.Body.String())
	assert.Contains(t, res.Body.String(), "Restart AgentsView the way you started it")

	f.srv.mu.Lock()
	f.srv.cfg.Host = "example"
	f.srv.mu.Unlock()
	called := false
	f.srv.restartDaemon = func(ctx context.Context, cfg config.Config) error {
		called = true
		assert.Equal(t, "example", cfg.Host)
		assert.NoError(t, ctx.Err())
		return nil
	}
	assert.True(t, settings())
	// Restart must remain available when the archive writer is closed.
	require.NoError(t, f.db.CloseWriter())
	res = serveJSON(t, f.handler, http.MethodPost, "/api/v1/daemon/restart", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.True(t, called)

	f.srv.restartDaemon = func(context.Context, config.Config) error { return errors.New("launch failed") }
	res = serveJSON(t, f.handler, http.MethodPost, "/api/v1/daemon/restart", nil)
	assert.Equal(t, http.StatusInternalServerError, res.Code, res.Body.String())
}

// A settings save that arrives while a restart is being prepared must not be
// dropped: the replacement is built from the settings read at the start.
func TestDaemonRestartRefusesSettingsSavesItWouldDrop(t *testing.T) {
	requireAuth := func(t *testing.T, f *syncRouteFixture) bool {
		t.Helper()
		res := serveJSON(t, f.handler, http.MethodGet, "/api/v1/settings", nil)
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		var body struct {
			RequireAuth bool `json:"require_auth"`
		}
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
		return body.RequireAuth
	}
	t.Run("save during an accepted restart", func(t *testing.T) {
		f := newSyncRouteFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		f.srv.restartDaemon = func(context.Context, config.Config) error {
			close(entered)
			<-release
			return nil
		}
		// The requests run outside the test goroutine, so they serve raw
		// recorders and the test asserts on the results.
		request := func(method, path, body string) *http.Request {
			req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
			req.Host = "127.0.0.1:0"
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Origin", "http://127.0.0.1:0")
			req.Header.Set("Content-Type", "application/json")
			return req
		}
		restarted := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, request(http.MethodPost, "/api/v1/daemon/restart", ""))
			restarted <- w
		}()
		<-entered
		saved := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, request(http.MethodPut, "/api/v1/settings", `{"require_auth":true}`))
			saved <- w
		}()
		select {
		case w := <-saved:
			require.FailNow(t, "settings save finished while the restart was being prepared", "status %d: %s", w.Code, w.Body.String())
		case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: absence check; the restart holds the settings lock, so the save must not finish before release
		}
		close(release)
		res := <-restarted
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		res = <-saved
		assert.Equal(t, http.StatusConflict, res.Code, res.Body.String())
		assert.False(t, requireAuth(t, f))
	})

	t.Run("save after a refused restart", func(t *testing.T) {
		f := newSyncRouteFixture(t)
		f.srv.restartDaemon = func(context.Context, config.Config) error { return errors.New("launch failed") }
		res := serveJSON(t, f.handler, http.MethodPost, "/api/v1/daemon/restart", nil)
		require.Equal(t, http.StatusInternalServerError, res.Code, res.Body.String())
		res = serveJSON(t, f.handler, http.MethodPut, "/api/v1/settings", map[string]any{"zoom_level": 110})
		assert.Equal(t, http.StatusOK, res.Code, res.Body.String())
	})
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
