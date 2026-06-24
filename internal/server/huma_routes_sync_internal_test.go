package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	"go.kenn.io/agentsview/internal/parser"
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
		database, err := db.Open(dbPath)
		require.NoError(t, err)
		require.NoError(t, database.Close())
		markDBStale(t, dbPath)
	}

	database, err := db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

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

func TestSyncEngineForLocalReusesNoSyncEngineConcurrently(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

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
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

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
	)

	assert.Empty(t, response.Failures)
	select {
	case ev := <-events:
		assert.Equal(t, "sessions", ev.Scope)
	case <-time.After(time.Second):
		require.FailNow(t, "remote sync did not emit")
	}
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
