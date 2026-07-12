package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	stdlibsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/ssh"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type syncRouteFixture struct {
	dir       string
	dbPath    string
	claudeDir string
	db        *db.DB
	srv       *Server
	handler   http.Handler
}

type syncRouteFixtureConfig struct {
	stale       bool
	remoteHosts []config.RemoteHost
	broadcaster *Broadcaster
	engine      *syncpkg.Engine
}

type syncRouteFixtureOption func(*syncRouteFixtureConfig)

func withStaleDB() syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.stale = true }
}

func withRemoteHosts(hosts ...config.RemoteHost) syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.remoteHosts = hosts }
}

func withBroadcasterForSyncRoutes(b *Broadcaster) syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.broadcaster = b }
}

func newSyncRouteFixture(
	t *testing.T,
	opts ...syncRouteFixtureOption,
) *syncRouteFixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	claudeDir := filepath.Join(dir, "claude")

	var cfg syncRouteFixtureConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.stale {
		dbtest.EnsureTestDBAt(t, dbPath)
		markDBStale(t, dbPath)
	}

	var database *db.DB
	var err error
	if cfg.stale {
		database, err = db.Open(dbPath)
		require.NoError(t, err)
		t.Cleanup(func() { database.Close() })
	} else {
		database = dbtest.OpenTestDBAt(t, dbPath)
	}

	serverConfig := config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		DBPath:       dbPath,
		WriteTimeout: 30 * time.Second,
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
		RemoteHosts: cfg.remoteHosts,
	}
	var serverOptions []Option
	if cfg.broadcaster != nil {
		serverOptions = append(serverOptions, WithBroadcaster(cfg.broadcaster))
	}
	srv := New(serverConfig, database, cfg.engine, serverOptions...)
	return &syncRouteFixture{
		dir:       dir,
		dbPath:    dbPath,
		claudeDir: claudeDir,
		db:        database,
		srv:       srv,
		handler:   srv.Handler(),
	}
}

func (f *syncRouteFixture) writeClaudeSession(
	t *testing.T,
	relPath string,
	firstMessage string,
) string {
	t.Helper()
	sessionPath := filepath.Join(f.claudeDir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(
		sessionPath,
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", firstMessage).
			String()),
		0o644,
	))
	return sessionPath
}

func markDBStale(t *testing.T, dbPath string) {
	t.Helper()
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.Exec("PRAGMA user_version = 0")
	require.NoError(t, err)
	require.NoError(t, raw.Close())
}

type syncRouteRequestOption func(*http.Request)

func withRemoteAddr(addr string) syncRouteRequestOption {
	return func(req *http.Request) { req.RemoteAddr = addr }
}

func withAccept(value string) syncRouteRequestOption {
	return func(req *http.Request) { req.Header.Set("Accept", value) }
}

func serveJSON(
	t *testing.T,
	h http.Handler,
	method string,
	path string,
	body any,
	opts ...syncRouteRequestOption,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(payload)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Origin", "http://127.0.0.1:0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func postSessionSync(
	t *testing.T,
	h http.Handler,
	sessionPath string,
) *httptest.ResponseRecorder {
	t.Helper()
	return serveJSON(t, h, http.MethodPost, "/api/v1/sessions/sync",
		service.SyncInput{Path: sessionPath})
}

func postRemoteSync(
	t *testing.T,
	h http.Handler,
	hosts []config.RemoteHost,
	opts ...syncRouteRequestOption,
) *httptest.ResponseRecorder {
	t.Helper()
	return serveJSON(t, h, http.MethodPost, "/api/v1/sync/remotes",
		remoteSyncRequest{Hosts: hosts}, opts...)
}

func decodeRecorder[T any](
	t *testing.T,
	w *httptest.ResponseRecorder,
) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

func assertFirstMessageContains(t *testing.T, msg *string, want string) {
	t.Helper()
	require.NotNil(t, msg)
	assert.Contains(t, *msg, want)
}

func assertOnlySessionFirstMessageContains(
	t *testing.T,
	database *db.DB,
	want string,
) {
	t.Helper()
	page, err := database.ListSessions(context.Background(), db.SessionFilter{
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assertFirstMessageContains(t, page.Sessions[0].FirstMessage, want)
}

func stubRunRemoteSync(
	t *testing.T,
	fn func(context.Context, *ssh.RemoteSync) (ssh.SyncStats, error),
) {
	t.Helper()
	originalRunRemoteSync := runRemoteSync
	runRemoteSync = fn
	t.Cleanup(func() { runRemoteSync = originalRunRemoteSync })
}

func stubRunHTTPRemoteSync(
	t *testing.T,
	fn func(context.Context, config.RemoteHost, bool) (remotesync.SyncStats, error),
) {
	t.Helper()
	originalRunHTTPRemoteSync := runHTTPRemoteSync
	runHTTPRemoteSync = func(
		ctx context.Context,
		_ config.Config,
		_ *db.DB,
		rh config.RemoteHost,
		full bool,
		_ func(syncpkg.Progress),
	) (remotesync.SyncStats, error) {
		return fn(ctx, rh, full)
	}
	t.Cleanup(func() { runHTTPRemoteSync = originalRunHTTPRemoteSync })
}

func TestSyncEngineForLocalReusesNoSyncEngineConcurrently(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database := dbtest.OpenTestDBAt(t, dbPath)

	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		DBPath:       dbPath,
		WriteTimeout: 30 * time.Second,
	}, database, nil)

	const workers = 8
	engines := make([]*syncpkg.Engine, workers)
	var wg stdlibsync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func() {
			defer wg.Done()
			engines[i] = srv.syncEngineForLocal(database)
		}()
	}
	wg.Wait()

	require.NotNil(t, engines[0])
	for _, engine := range engines[1:] {
		assert.Same(t, engines[0], engine)
	}
}

func TestHumaSyncStatusUsesExistingOnDemandEngine(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database := dbtest.OpenTestDBAt(t, dbPath)

	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		DBPath:       dbPath,
		WriteTimeout: 30 * time.Second,
	}, database, nil)
	engine := srv.syncEngineForLocal(database)
	engine.SyncAll(context.Background(), nil)

	out, err := srv.humaSyncStatus(context.Background(), &emptyInput{})

	require.NoError(t, err)
	require.NotNil(t, out.Body.Stats)
	assert.Equal(t, engine.LastSyncStats(), *out.Body.Stats)
}

func TestHumaSyncSessionLocalNoSyncUsesOnDemandEngine(t *testing.T) {
	f := newSyncRouteFixture(t)
	sessionPath := f.writeClaudeSession(t, "proj/session.jsonl", "no sync route")
	w := postSessionSync(t, f.handler, sessionPath)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	detail := decodeRecorder[service.SessionDetail](t, w)
	assert.Equal(t, "claude", detail.Agent)
	assertFirstMessageContains(t, detail.FirstMessage, "no sync route")
}

func TestHumaSyncSessionRouteIsNotWriteTimeoutWrapped(t *testing.T) {
	srv := testServer(
		t, 10*time.Millisecond,
		withHandlerDelay(100*time.Millisecond),
	)
	w := serveJSON(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/sync",
		map[string]any{})

	resp := w.Result()
	defer resp.Body.Close()
	assert.False(t, isTimeoutResponse(t, resp))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHumaTriggerSyncLocalNoSyncResyncsStaleDB(t *testing.T) {
	f := newSyncRouteFixture(t, withStaleDB())
	f.writeClaudeSession(t, "proj/session.jsonl", "stale no sync route")
	require.True(t, f.db.NeedsResync())
	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync", nil)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.False(t, f.db.NeedsResync())
	assertOnlySessionFirstMessageContains(t, f.db, "stale no sync route")
}

func TestForegroundSyncReleasesDeferredStartupMaintenance(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Server, *syncpkg.Engine)
	}{
		{
			name: "sync",
			run: func(srv *Server, engine *syncpkg.Engine) {
				srv.runSyncWithResyncFallback(
					context.Background(), engine, nil,
				)
			},
		},
		{
			name: "resync",
			run: func(srv *Server, engine *syncpkg.Engine) {
				srv.runResyncWithFallback(
					context.Background(), engine, nil,
				)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
				Machine:                 "local",
				DeferStartupMaintenance: true,
			})
			t.Cleanup(engine.Close)
			srv := &Server{db: database}

			maintenanceStarted := make(chan struct{})
			maintenanceDone := make(chan error, 1)
			go func() {
				maintenanceDone <- engine.RunStartupMaintenance(
					t.Context(),
					func() error {
						close(maintenanceStarted)
						return nil
					},
				)
			}()
			assert.Never(t, func() bool {
				select {
				case <-maintenanceStarted:
					return true
				default:
					return false
				}
			}, 100*time.Millisecond, 10*time.Millisecond,
				"maintenance started before foreground synchronization")

			tt.run(srv, engine)
			select {
			case err := <-maintenanceDone:
				require.NoError(t, err)
			case <-time.After(time.Second):
				require.FailNow(t,
					"foreground synchronization did not release maintenance")
			}
		})
	}
}

func TestCanceledForegroundSyncLeavesStartupFallbackEligible(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		Machine:                 "local",
		DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)
	srv := &Server{db: database}

	requestCtx, cancelRequest := context.WithCancel(t.Context())
	cancelRequest()
	srv.runSyncWithResyncFallback(requestCtx, engine, nil)

	_, ran, err := engine.RunStartupSyncFallback(t.Context(), nil)
	require.NoError(t, err)
	assert.True(t, ran,
		"a canceled HTTP sync must leave daemon startup recovery eligible")
}

func TestHumaSyncSessionLocalNoSyncResyncsStaleDB(t *testing.T) {
	f := newSyncRouteFixture(t, withStaleDB())
	sessionPath := f.writeClaudeSession(t, "proj/session.jsonl",
		"stale session sync route")
	require.True(t, f.db.NeedsResync())
	w := postSessionSync(t, f.handler, sessionPath)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.False(t, f.db.NeedsResync())
	detail := decodeRecorder[service.SessionDetail](t, w)
	assertFirstMessageContains(t, detail.FirstMessage, "stale session sync route")
}

func TestHumaSyncSessionCanceledPreResyncReturnsNil(t *testing.T) {
	f := newSyncRouteFixture(t, withStaleDB())
	require.True(t, f.db.NeedsResync())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := f.srv.humaSyncSession(ctx, &sessionSyncInput{
		Body: service.SyncInput{Path: filepath.Join(f.dir, "missing.jsonl")},
	})

	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestHumaSyncSessionCanceledServiceSyncReturnsNil(t *testing.T) {
	f := newSyncRouteFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := f.srv.humaSyncSession(ctx, &sessionSyncInput{
		Body: service.SyncInput{Path: filepath.Join(f.dir, "missing.jsonl")},
	})

	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestRunRemoteSyncRequestEmitsAfterRemoteOnlyWrites(t *testing.T) {
	broadcaster := NewBroadcaster(0)
	f := newSyncRouteFixture(t, withBroadcasterForSyncRoutes(broadcaster))
	engine := f.srv.syncEngineForLocal(f.db)
	stubRunRemoteSync(t, func(
		context.Context,
		*ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		return ssh.SyncStats{SessionsSynced: 1}, nil
	})

	events, unsubscribe := broadcaster.Subscribe()
	t.Cleanup(unsubscribe)

	response := f.srv.runRemoteSyncRequest(
		context.Background(),
		f.db,
		engine,
		remoteSyncRequest{
			Hosts: []config.RemoteHost{{Host: "alpha"}},
		},
		nil,
	)

	assert.Empty(t, response.Failures)
	select {
	case ev := <-events:
		assert.Equal(t, "sessions", ev.Scope)
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync did not emit")
	}
}

func TestRunRemoteSyncHostsDispatchesHTTPTransport(t *testing.T) {
	f := newSyncRouteFixture(t)
	sshCalled := false
	stubRunRemoteSync(t, func(
		context.Context,
		*ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		sshCalled = true
		return ssh.SyncStats{}, errors.New("ssh runner called")
	})
	var got config.RemoteHost
	stubRunHTTPRemoteSync(t, func(
		_ context.Context,
		rh config.RemoteHost,
		_ bool,
	) (remotesync.SyncStats, error) {
		got = rh
		return remotesync.SyncStats{SessionsSynced: 1, SessionsTotal: 1}, nil
	})

	failures, stats := f.srv.runRemoteSyncHosts(
		context.Background(),
		f.db,
		[]config.RemoteHost{{
			Host:      "alpha",
			Transport: config.RemoteTransportHTTP,
			URL:       "https://alpha.example.test",
		}},
		false,
		nil,
	)

	assert.Empty(t, failures)
	assert.False(t, sshCalled, "server HTTP remote must not use SSH runner")
	assert.Equal(t, "https://alpha.example.test", got.URL)
	assert.Equal(t, remotesync.SyncStats{SessionsSynced: 1, SessionsTotal: 1}, stats)
}

func TestHumaSyncRemotesStreamsLocalProgress(t *testing.T) {
	f := newSyncRouteFixture(t)
	f.writeClaudeSession(t, "remote-progress.jsonl", "remote progress")
	stubRunRemoteSync(t, func(
		context.Context,
		*ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		return ssh.SyncStats{}, nil
	})

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync/remotes",
		remoteSyncRequest{
			Full:         true,
			IncludeLocal: true,
			Hosts:        []config.RemoteHost{{Host: "alpha"}},
		},
		withAccept("text/event-stream"),
	)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/event-stream")
	body := w.Body.String()
	assert.Contains(t, body, "event: progress")
	assert.Contains(t, body, `"resync":true`)
	assert.Contains(t, body, "event: done")
	assert.Contains(t, body, `"local_stats"`)
}

func TestHumaSyncRemotesStreamsRemoteProgress(t *testing.T) {
	f := newSyncRouteFixture(t)
	stubRunRemoteSync(t, func(
		_ context.Context,
		rs *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		require.NotNil(t, rs.Progress)
		rs.Progress(syncpkg.Progress{
			Detail: "Resolving agent directories on alpha",
		})
		return ssh.SyncStats{SessionsSynced: 1, SessionsTotal: 1}, nil
	})

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync/remotes",
		remoteSyncRequest{
			Hosts: []config.RemoteHost{{Host: "alpha"}},
		},
		withAccept("text/event-stream"),
	)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/event-stream")
	body := w.Body.String()
	assert.Contains(t, body, "event: progress")
	assert.Contains(t, body, "Resolving agent directories on alpha")
	assert.Contains(t, body, "event: done")
}

func TestRunRemoteSyncRequestSerializesNoSyncRemoteWrites(t *testing.T) {
	f := newSyncRouteFixture(t)
	engine := f.srv.syncEngineForLocal(f.db)

	remoteEntered := make(chan struct{})
	releaseRemote := make(chan struct{})
	var remoteOnce stdlibsync.Once
	stubRunRemoteSync(t, func(
		context.Context,
		*ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		remoteOnce.Do(func() { close(remoteEntered) })
		<-releaseRemote
		return ssh.SyncStats{}, nil
	})

	responseCh := make(chan remoteSyncResponse, 1)
	go func() {
		responseCh <- f.srv.runRemoteSyncRequest(
			context.Background(),
			f.db,
			engine,
			remoteSyncRequest{
				Hosts: []config.RemoteHost{{Host: "alpha"}},
			},
			nil,
		)
	}()

	select {
	case <-remoteEntered:
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync did not enter")
	}

	exclusiveEntered := make(chan struct{})
	exclusiveErr := make(chan error, 1)
	go func() {
		exclusiveErr <- engine.RunExclusive(func() error {
			close(exclusiveEntered)
			return nil
		})
	}()

	select {
	case <-exclusiveEntered:
		assert.Fail(t, "exclusive operation overlapped remote sync")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRemote)

	select {
	case response := <-responseCh:
		assert.Empty(t, response.Failures)
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync did not finish")
	}
	select {
	case err := <-exclusiveErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		require.FailNow(t, "exclusive operation did not finish")
	}
}

func TestHumaSyncRemotesRejectsOptionShapedHost(t *testing.T) {
	srv := testServer(t, 30)
	w := postRemoteSync(t, srv.Handler(),
		[]config.RemoteHost{{Host: "-oProxyCommand=sh"}})

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "host must not begin with '-'")
}

func TestHumaSyncRemotesRejectsNonLocalUnconfiguredHost(t *testing.T) {
	srv := testServer(t, 30)
	w := postRemoteSync(t, srv.Handler(),
		[]config.RemoteHost{{Host: "attacker-box"}},
		withRemoteAddr("192.168.1.50:1234"))

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "not configured in remote_hosts")
}

func TestHumaSyncRemotesAllowsNonLocalConfiguredExactHost(t *testing.T) {
	allowed := config.RemoteHost{Host: "allowed-box", User: "alice", Port: 2222}
	f := newSyncRouteFixture(t, withRemoteHosts(allowed))

	var got *ssh.RemoteSync
	stubRunRemoteSync(t, func(
		_ context.Context,
		rs *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		got = rs
		return ssh.SyncStats{SessionsSynced: 1, SessionsTotal: 1}, nil
	})
	w := postRemoteSync(t, f.handler, []config.RemoteHost{allowed},
		withRemoteAddr("192.168.1.50:1234"))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NotNil(t, got)
	assert.Equal(t, allowed.Host, got.Host)
	assert.Equal(t, allowed.User, got.User)
	assert.Equal(t, allowed.Port, got.Port)
}

func TestHumaSyncRemotesAllowsNonLocalConfiguredHostIgnoringInterval(t *testing.T) {
	allowed := config.RemoteHost{
		Host:     "allowed-box",
		User:     "alice",
		Port:     2222,
		Interval: 5 * time.Minute,
	}
	requested := config.RemoteHost{
		Host: "allowed-box",
		User: "alice",
		Port: 2222,
	}
	f := newSyncRouteFixture(t, withRemoteHosts(allowed))

	var got *ssh.RemoteSync
	stubRunRemoteSync(t, func(
		_ context.Context,
		rs *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		got = rs
		return ssh.SyncStats{SessionsSynced: 1, SessionsTotal: 1}, nil
	})
	w := postRemoteSync(t, f.handler, []config.RemoteHost{requested},
		withRemoteAddr("192.168.1.50:1234"))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NotNil(t, got)
	assert.Equal(t, requested.Host, got.Host)
	assert.Equal(t, requested.User, got.User)
	assert.Equal(t, requested.Port, got.Port)
}

func TestSyncRemotesUsesStoredConfigForConfiguredHost(t *testing.T) {
	stored := config.RemoteHost{
		Host:      "devbox",
		Transport: config.RemoteTransportHTTP,
		URL:       "http://stored.example",
		Token:     "stored-token",
	}
	f := newSyncRouteFixture(t, withRemoteHosts(stored))
	var got config.RemoteHost
	stubRunHTTPRemoteSync(t, func(
		_ context.Context,
		rh config.RemoteHost,
		_ bool,
	) (remotesync.SyncStats, error) {
		got = rh
		return remotesync.SyncStats{}, nil
	})

	w := postRemoteSync(t, f.handler, []config.RemoteHost{{
		Host:      "devbox",
		Transport: config.RemoteTransportHTTP,
		URL:       "http://169.254.169.254",
		Token:     "evil",
	}}, withRemoteAddr("203.0.113.10:9999"))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, stored.URL, got.URL)
	assert.Equal(t, stored.Token, got.Token)
}

func TestSyncRemotesRedactsStoredHTTPConfigOnFailure(t *testing.T) {
	stored := config.RemoteHost{
		Host:      "devbox",
		Transport: config.RemoteTransportHTTP,
		URL:       "http://stored.example",
		Token:     "stored.example-secret",
	}
	f := newSyncRouteFixture(t, withRemoteHosts(stored))
	stubRunHTTPRemoteSync(t, func(
		_ context.Context,
		_ config.RemoteHost,
		_ bool,
	) (remotesync.SyncStats, error) {
		return remotesync.SyncStats{}, errors.New(
			`Get "http://stored.example/api/v1/remote-sync/targets": lookup stored.example: bearer stored.example-secret rejected`,
		)
	})

	w := postRemoteSync(t, f.handler,
		[]config.RemoteHost{{Host: "devbox"}},
		withRemoteAddr("203.0.113.10:9999"))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "stored.example-secret")
	assert.NotContains(t, w.Body.String(), "secret")
	assert.NotContains(t, w.Body.String(), "stored.example")
	resp := decodeRecorder[remoteSyncResponse](t, w)
	require.Len(t, resp.Failures, 1)
	assert.Equal(t, config.RemoteHost{Host: "devbox"}, resp.Failures[0].Host)
	assert.NotContains(t, resp.Failures[0].Err, "stored.example")
	assert.NotContains(t, resp.Failures[0].Err, "secret")
	assert.Equal(t, "HTTP remote sync failed", resp.Failures[0].Err)
}

func TestSyncRemotesClassifiesHTTPFailures(t *testing.T) {
	stored := config.RemoteHost{
		Host:      "devbox",
		Transport: config.RemoteTransportHTTP,
		URL:       "http://stored.example",
		Token:     "stored.example-secret",
	}
	f := newSyncRouteFixture(t, withRemoteHosts(stored))
	stubRunHTTPRemoteSync(t, func(
		_ context.Context,
		_ config.RemoteHost,
		_ bool,
	) (remotesync.SyncStats, error) {
		return remotesync.SyncStats{}, fmt.Errorf(
			"fetch targets: %w", &remotesync.StatusError{
				Code:   401,
				Status: "401 Unauthorized",
				Detail: "bearer stored.example-secret rejected",
			},
		)
	})

	w := postRemoteSync(t, f.handler,
		[]config.RemoteHost{{Host: "devbox"}},
		withRemoteAddr("203.0.113.10:9999"))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeRecorder[remoteSyncResponse](t, w)
	require.Len(t, resp.Failures, 1)
	assert.Contains(t, resp.Failures[0].Err,
		"rejected the sync token (401 Unauthorized)")
	assert.Contains(t, resp.Failures[0].Err,
		"must match the remote daemon's auth_token")
	assert.NotContains(t, resp.Failures[0].Err, "stored.example",
		"response body detail must not leak")
}

func TestRunHTTPRemoteSyncRequiresExplicitHTTPToken(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(ts.Close)

	_, err := runHTTPRemoteSync(
		context.Background(),
		config.Config{AuthToken: "collector-token"},
		nil,
		config.RemoteHost{
			Host:      "devbox",
			Transport: config.RemoteTransportHTTP,
			URL:       ts.URL,
		},
		false,
		nil,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "token is required")
	assert.False(t, called, "collector auth_token must not be sent to remote")
}

func TestSyncRemotesRejectsAdHocHTTP(t *testing.T) {
	f := newSyncRouteFixture(t)
	w := postRemoteSync(t, f.handler, []config.RemoteHost{{
		Host:      "devbox",
		Transport: config.RemoteTransportHTTP,
		URL:       "http://devbox:8080",
	}})

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRunHTTPRemoteSyncReachesMirrorPath(t *testing.T) {
	manifestRequests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case "/api/v1/remote-sync/manifest":
			manifestRequests++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	database := dbtest.OpenTestDB(t)

	_, err := runHTTPRemoteSync(
		context.Background(),
		config.Config{DataDir: t.TempDir()},
		database,
		config.RemoteHost{
			Host:      "devbox",
			Transport: config.RemoteTransportHTTP,
			URL:       ts.URL,
			Token:     "remote-token",
		},
		false,
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, manifestRequests,
		"configured DataDir must route HTTP sync through the manifest/mirror path")
}
