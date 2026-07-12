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
	"net/url"
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

func assertSessionCount(t *testing.T, database *db.DB, want int) {
	t.Helper()
	page, err := database.ListSessions(context.Background(), db.SessionFilter{
		Limit: 10,
	})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, want)
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

type fakePreparedHTTPRebuild struct {
	contributors []syncpkg.RebuildContributor
	closed       int
	closeErrors  []error
}

func (p *fakePreparedHTTPRebuild) BorrowRebuildContributors() (
	[]syncpkg.RebuildContributor, func(), error,
) {
	return p.contributors, func() {}, nil
}

func (p *fakePreparedHTTPRebuild) Close() error {
	p.closed++
	if p.closed <= len(p.closeErrors) {
		return p.closeErrors[p.closed-1]
	}
	return nil
}

func TestHTTPCoordinatorFailurePrefersContributorOverCleanupHost(t *testing.T) {
	contributorCause := errors.New("alpha contributor failed")
	cleanupCause := errors.New("beta cleanup failed")
	err := errors.Join(
		&syncpkg.RebuildContributorError{
			Contributor: "alpha", Err: contributorCause,
		},
		&remotesync.HostError{
			Host: "beta", Operation: "cleanup", Err: cleanupCause,
		},
	)

	failure, ok := httpCoordinatorFailure([]config.RemoteHost{
		{Host: "alpha", Transport: config.RemoteTransportHTTP},
		{Host: "beta", Transport: config.RemoteTransportHTTP},
	}, err)

	require.True(t, ok)
	assert.Equal(t, "alpha", failure.Host.Host)
	assert.Equal(t, remotesync.FailureSummary(contributorCause), failure.Err)
}

type failingRebuildCleanup struct {
	errors []error
	calls  int
}

type lockOrderCleanup struct {
	engine       *syncpkg.Engine
	probeEntered chan struct{}
	releaseProbe chan struct{}
	calls        int
}

func (c *lockOrderCleanup) Error() string { return "pending cleanup" }

func (c *lockOrderCleanup) RetryCleanup() error {
	c.calls++
	if c.calls == 1 {
		return errors.New("retain pending cleanup")
	}
	return c.engine.RunExclusive(func() error {
		close(c.probeEntered)
		<-c.releaseProbe
		return nil
	})
}

func TestRunRemoteSyncRequestHTTPPathsAcquireCleanupBeforeEngine(t *testing.T) {
	f := newSyncRouteFixture(t)
	engine := f.srv.syncEngineForLocal(f.db)
	owner := &lockOrderCleanup{
		engine:       engine,
		probeEntered: make(chan struct{}),
		releaseProbe: make(chan struct{}),
	}
	_, seedErr := f.srv.httpRemoteCleanupRegistry.Run(
		func() (remotesync.SyncStats, error) {
			return remotesync.SyncStats{}, owner
		},
	)
	require.Error(t, seedErr)

	var mu stdlibsync.Mutex
	var callbacks []string
	stubRunHTTPRemoteSync(t, func(
		_ context.Context, rh config.RemoteHost, _ bool,
	) (remotesync.SyncStats, error) {
		mu.Lock()
		callbacks = append(callbacks, rh.Host)
		mu.Unlock()
		return remotesync.SyncStats{}, nil
	})
	host := func(name string) config.RemoteHost {
		return config.RemoteHost{Host: name, Transport: config.RemoteTransportHTTP}
	}

	remoteOnlyDone := make(chan remoteSyncResponse, 1)
	go func() {
		remoteOnlyDone <- f.srv.runRemoteSyncRequest(
			context.Background(), f.db, engine,
			remoteSyncRequest{Hosts: []config.RemoteHost{host("remote-only")}}, nil,
		)
	}()

	select {
	case <-owner.probeEntered:
	case <-time.After(time.Second):
		require.FailNow(t, "cleanup retry could not acquire engine")
	}

	includeLocalDone := make(chan remoteSyncResponse, 1)
	go func() {
		includeLocalDone <- f.srv.runRemoteSyncRequest(
			context.Background(), f.db, engine,
			remoteSyncRequest{
				IncludeLocal: true,
				Hosts:        []config.RemoteHost{host("include-local")},
			}, nil,
		)
	}()
	close(owner.releaseProbe)

	for _, done := range []chan remoteSyncResponse{remoteOnlyDone, includeLocalDone} {
		select {
		case response := <-done:
			assert.Empty(t, response.Failures)
		case <-time.After(time.Second):
			require.FailNow(t, "HTTP request did not complete")
		}
	}
	mu.Lock()
	assert.Equal(t, []string{"remote-only", "include-local"}, callbacks)
	mu.Unlock()
}

func (c *failingRebuildCleanup) Close() error {
	c.calls++
	if c.calls <= len(c.errors) {
		return c.errors[c.calls-1]
	}
	return nil
}

func TestRebuildCleanupFailureIsRetainedByHTTPCleanupRegistry(t *testing.T) {
	f := newSyncRouteFixture(t)
	engine := f.srv.syncEngineForLocal(f.db)
	cleanup := &failingRebuildCleanup{errors: []error{
		errors.New("deferred close failed"),
		errors.New("immediate retry failed"),
	}}
	registry := remotesync.CleanupRegistry{}

	_, firstErr := registry.Run(func() (remotesync.SyncStats, error) {
		_, err := engine.SyncThenRunWithRebuild(
			context.Background(), true, nil,
			func() (syncpkg.RebuildOptions, syncpkg.RebuildCleanup, error) {
				return syncpkg.RebuildOptions{}, cleanup, nil
			},
			func(bool, bool) error { return nil },
		)
		return remotesync.SyncStats{}, err
	})
	require.Error(t, firstErr)
	assert.Equal(t, 2, cleanup.calls,
		"registry must immediately retry the failed deferred close")

	nextRan := false
	_, secondErr := registry.Run(func() (remotesync.SyncStats, error) {
		nextRan = true
		return remotesync.SyncStats{}, nil
	})
	require.NoError(t, secondErr)
	assert.True(t, nextRan)
	assert.Equal(t, 3, cleanup.calls,
		"retained cleanup must finish before the next callback")
}

func stubPrepareHTTPRebuild(
	t *testing.T,
	fn func(context.Context, []remotesync.HTTPSync) (preparedHTTPRebuild, error),
) {
	t.Helper()
	original := prepareHTTPRebuild
	prepareHTTPRebuild = fn
	t.Cleanup(func() { prepareHTTPRebuild = original })
}

func TestRunRemoteSyncRequestUnifiedHTTPContributorFailureSkipsSSH(t *testing.T) {
	f := newSyncRouteFixture(t)
	f.writeClaudeSession(t, "proj/local.jsonl", "local survives contributor failure")
	assertSessionCount(t, f.db, 0)

	sentinel := errors.New("persist remote cache")
	prepared := &fakePreparedHTTPRebuild{contributors: []syncpkg.RebuildContributor{{
		Name:      "alpha",
		AfterSync: func(*syncpkg.Engine, *db.DB) error { return sentinel },
	}}}
	stubPrepareHTTPRebuild(t, func(
		_ context.Context, syncs []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		require.Len(t, syncs, 1)
		assert.Equal(t, "alpha", syncs[0].Host)
		return prepared, nil
	})
	sshCalls := 0
	stubRunRemoteSync(t, func(
		context.Context, *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		sshCalls++
		return ssh.SyncStats{}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
				{Host: "beta", Transport: config.RemoteTransportSSH},
			},
		}, nil,
	)

	require.Len(t, response.Failures, 1)
	assert.Equal(t, "alpha", response.Failures[0].Host.Host)
	assert.NotContains(t, response.Failures[0].Err, sentinel.Error())
	assert.Zero(t, sshCalls)
	assert.Equal(t, 1, prepared.closed)
	assertSessionCount(t, f.db, 0)
}

func TestRunRemoteSyncRequestContributorFailurePrecedesRetainedCleanupHost(t *testing.T) {
	f := newSyncRouteFixture(t)
	contributorCause := &remotesync.StatusError{
		Code: http.StatusForbidden, Detail: "private contributor detail",
	}
	cleanupCause := errors.New("beta mirror cleanup failed")
	cleanupErr := &remotesync.HostError{
		Host: "beta", Operation: "cleanup", Err: cleanupCause,
	}
	prepared := &fakePreparedHTTPRebuild{
		contributors: []syncpkg.RebuildContributor{{
			Name: "alpha",
			AfterSync: func(*syncpkg.Engine, *db.DB) error {
				return contributorCause
			},
		}},
		closeErrors: []error{cleanupErr, cleanupErr},
	}
	stubPrepareHTTPRebuild(t, func(
		_ context.Context, syncs []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		require.Len(t, syncs, 2)
		return prepared, nil
	})
	activeCalls := 0
	stubRunHTTPRemoteSync(t, func(
		context.Context, config.RemoteHost, bool,
	) (remotesync.SyncStats, error) {
		activeCalls++
		return remotesync.SyncStats{}, nil
	})
	httpHost := func(host string) config.RemoteHost {
		return config.RemoteHost{
			Host: host, Transport: config.RemoteTransportHTTP, Token: "secret",
		}
	}

	first := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{httpHost("alpha"), httpHost("beta")},
		}, nil,
	)
	require.Len(t, first.Failures, 1)
	assert.Equal(t, "alpha", first.Failures[0].Host.Host)
	assert.Contains(t, first.Failures[0].Err, "403 Forbidden")
	assert.NotContains(t, first.Failures[0].Err, contributorCause.Detail)
	assert.Equal(t, 2, prepared.closed,
		"failed deferred cleanup must be retried and retained")
	assert.Zero(t, activeCalls)

	second := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{Hosts: []config.RemoteHost{httpHost("gamma")}}, nil,
	)
	assert.Empty(t, second.Failures)
	assert.Equal(t, 3, prepared.closed,
		"next request must release retained beta cleanup first")
	assert.Equal(t, 1, activeCalls)
}

func TestRunRemoteSyncRequestUnifiedHTTPUsesMirrorDeltaAndBulkRebuild(t *testing.T) {
	broadcaster := NewBroadcaster(0)
	f := newSyncRouteFixture(t, withBroadcasterForSyncRoutes(broadcaster))
	events, unsubscribe := broadcaster.Subscribe()
	t.Cleanup(unsubscribe)
	f.writeClaudeSession(t, "proj/local.jsonl", "unified local")
	remoteDir := filepath.Join(t.TempDir(), "remote-claude")
	remotePath := filepath.Join(remoteDir, "project", "remote.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(remotePath), 0o755))
	require.NoError(t, os.WriteFile(
		remotePath,
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "unified remote").
			String()),
		0o644,
	))
	targets := remotesync.TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentClaude: {remoteDir},
	}}
	archiveRequests := 0
	serverErrors := make(chan error, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(targets); err != nil {
				serverErrors <- err
			}
		case "/api/v1/remote-sync/manifest":
			manifest, err := remotesync.BuildManifest(targets)
			if err != nil {
				serverErrors <- err
				http.Error(w, "manifest failed", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(manifest); err != nil {
				serverErrors <- err
			}
		case "/api/v1/remote-sync/archive":
			var request remotesync.ArchiveRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				serverErrors <- err
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			archiveRequests++
			w.Header().Set("Content-Type", "application/x-tar")
			var err error
			if request.DeltaFiles == nil {
				err = remotesync.WriteArchive(w, request.TargetSet)
			} else {
				err = remotesync.WriteArchiveFiles(
					w, targets.DeltaAllowedRoots(), request.DeltaFiles,
				)
			}
			if err != nil {
				serverErrors <- err
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	host := config.RemoteHost{
		Host: "alpha", Transport: config.RemoteTransportHTTP,
		URL: ts.URL, Token: "remote-token",
	}

	for range 2 {
		response := f.srv.runRemoteSyncRequest(
			context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
			remoteSyncRequest{
				Full: true, IncludeLocal: true, Hosts: []config.RemoteHost{host},
			}, nil,
		)
		assert.Empty(t, response.Failures)
		require.NotNil(t, response.LocalStats)
		assert.False(t, response.LocalStats.Aborted)
		assert.Equal(t, 2, response.LocalStats.Synced)
		select {
		case event := <-events:
			assert.Equal(t, "sync", event.Scope)
		case <-time.After(time.Second):
			require.FailNow(t, "unified rebuild did not emit a sync event")
		}
	}

	assert.Equal(t, 1, archiveRequests,
		"unchanged full rebuild should reuse the prepared mirror")
	assertSessionCount(t, f.db, 2)
	select {
	case err := <-serverErrors:
		require.NoError(t, err)
	default:
	}
}

func TestRunRemoteSyncRequestHTTPPreparationFailureSkipsSSHAndSwap(t *testing.T) {
	f := newSyncRouteFixture(t)
	f.writeClaudeSession(t, "proj/local.jsonl", "must remain outside active db")
	prepared := &fakePreparedHTTPRebuild{}
	remoteCause := &remotesync.StatusError{
		Code: http.StatusForbidden, Detail: "private response body",
	}
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		return prepared, &remotesync.HostError{
			Host: "alpha", Operation: "prepare", Err: remoteCause,
		}
	})
	sshCalls := 0
	stubRunRemoteSync(t, func(
		context.Context, *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		sshCalls++
		return ssh.SyncStats{}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
				{Host: "beta", Transport: config.RemoteTransportSSH},
			},
		}, nil,
	)

	require.Len(t, response.Failures, 1)
	assert.Equal(t, "alpha", response.Failures[0].Host.Host)
	assert.Contains(t, response.Failures[0].Err, "403 Forbidden")
	assert.NotContains(t, response.Failures[0].Err, remoteCause.Detail)
	assert.Zero(t, sshCalls)
	assert.Equal(t, 1, prepared.closed)
	assertSessionCount(t, f.db, 0)
}

func TestRunRemoteSyncRequestAbortedUnifiedRebuildReturnsTopLevelError(t *testing.T) {
	f := newSyncRouteFixture(t)
	missingPath := filepath.Join(f.dir, "missing.jsonl")
	require.NoError(t, f.db.UpsertSession(db.Session{
		ID: "preserved-old-session", Agent: "claude", Machine: "local",
		Project: "preserved", FilePath: &missingPath, MessageCount: 1,
	}))
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		return &fakePreparedHTTPRebuild{}, nil
	})
	sshCalls := 0
	stubRunRemoteSync(t, func(
		context.Context, *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		sshCalls++
		return ssh.SyncStats{}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
				{Host: "beta", Transport: config.RemoteTransportSSH},
			},
		}, nil,
	)

	require.NotNil(t, response.LocalStats)
	assert.True(t, response.LocalStats.Aborted)
	assert.Equal(t, "unified local and HTTP rebuild aborted", response.Error)
	assert.Empty(t, response.Failures,
		"an aggregate rebuild abort is not a host failure")
	assert.Zero(t, sshCalls)
	preserved, err := f.db.GetSession(context.Background(), "preserved-old-session")
	require.NoError(t, err)
	assert.NotNil(t, preserved)
}

func TestRemoteSyncTopLevelErrorDoesNotMislabelLocalFailure(t *testing.T) {
	localErr := errors.New("private local FTS failure")
	cleanupErr := &remotesync.HostError{
		Host: "alpha", Operation: "cleanup", Err: errors.New("mirror unlock failed"),
	}

	got := remoteSyncTopLevelError(errors.Join(localErr, cleanupErr))

	assert.Equal(t, "local sync failed", got)
	assert.NotContains(t, got, localErr.Error())
	assert.NotContains(t, got, "alpha")
}

func TestRemoteSyncTopLevelErrorReportsPendingCleanupBeforeWrappedCause(t *testing.T) {
	err := &remotesync.PendingCleanupError{Err: &remotesync.StatusError{
		Code: http.StatusForbidden, Detail: "private retained body",
	}}

	got := remoteSyncTopLevelError(err)

	assert.Equal(t,
		"HTTP remote sync blocked: cleanup from an earlier sync still owns resources",
		got,
	)
	assert.NotContains(t, got, "403")
	assert.NotContains(t, got, "private")
}

func TestServerUsesInjectedHTTPRemoteCleanupRegistry(t *testing.T) {
	f := newSyncRouteFixture(t)
	shared := new(remotesync.CleanupRegistry)
	srv := New(f.srv.cfg, f.db, nil,
		WithHTTPRemoteCleanupRegistry(shared))

	assert.Same(t, shared, srv.httpRemoteCleanupRegistry)
}

func TestRunRemoteSyncRequestCanceledRebuildReportsCancellation(t *testing.T) {
	f := newSyncRouteFixture(t)
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		return &fakePreparedHTTPRebuild{}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	response := f.srv.runRemoteSyncRequest(
		ctx, f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{{
				Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret",
			}},
		}, nil,
	)

	require.NotNil(t, response.LocalStats)
	assert.True(t, response.LocalStats.Aborted)
	assert.Equal(t, context.Canceled.Error(), response.Error)
	assert.NotEqual(t, syncpkg.ErrUnifiedRebuildAborted.Error(), response.Error)
	assert.Empty(t, response.Failures)
}

func TestRunRemoteSyncRequestSanitizesWrappedContextErrors(t *testing.T) {
	tests := []struct {
		name  string
		cause error
	}{
		{name: "canceled", cause: context.Canceled},
		{name: "deadline", cause: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newSyncRouteFixture(t)
			privateURL := "http://example.invalid/private/archive?token=secret-token"
			stubPrepareHTTPRebuild(t, func(
				context.Context, []remotesync.HTTPSync,
			) (preparedHTTPRebuild, error) {
				return nil, &url.Error{
					Op: "Get", URL: privateURL, Err: tt.cause,
				}
			})

			response := f.srv.runRemoteSyncRequest(
				context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
				remoteSyncRequest{
					Full: true, IncludeLocal: true,
					Hosts: []config.RemoteHost{{
						Host: "alpha", Transport: config.RemoteTransportHTTP,
						Token: "configured-token",
					}},
				}, nil,
			)

			assert.Equal(t, tt.cause.Error(), response.Error)
			assert.NotContains(t, response.Error, privateURL)
			assert.NotContains(t, response.Error, "secret-token")
			assert.NotContains(t, response.Error, "/private/archive")
			assert.Empty(t, response.Failures)
		})
	}
}

func TestRunRemoteSyncRequestMixedRunsSSHFullAfterUnifiedSwap(t *testing.T) {
	f := newSyncRouteFixture(t)
	f.writeClaudeSession(t, "proj/local.jsonl", "local swapped before ssh")
	order := make([]string, 0, 3)
	prepared := &fakePreparedHTTPRebuild{contributors: []syncpkg.RebuildContributor{{
		Name: "alpha",
		AfterSync: func(_ *syncpkg.Engine, database *db.DB) error {
			order = append(order, "http")
			return nil
		},
	}}}
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		order = append(order, "prepare")
		return prepared, nil
	})
	stubRunRemoteSync(t, func(
		_ context.Context, rs *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		order = append(order, "ssh")
		assert.True(t, rs.Full)
		assertOnlySessionFirstMessageContains(t, f.db, "local swapped before ssh")
		return ssh.SyncStats{}, errors.New("ssh unavailable")
	})

	response := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
				{Host: "beta", Transport: config.RemoteTransportSSH},
			},
		}, nil,
	)

	assert.Equal(t, []string{"prepare", "http", "ssh"}, order)
	require.Len(t, response.Failures, 1)
	assert.Equal(t, "beta", response.Failures[0].Host.Host)
	assertOnlySessionFirstMessageContains(t, f.db, "local swapped before ssh")
}

func TestRunRemoteSyncRequestFullSSHOnlyFallsBackBeforeRemoteImport(t *testing.T) {
	f := newSyncRouteFixture(t)
	missingPath := filepath.Join(f.dir, "missing-remote.jsonl")
	require.NoError(t, f.db.UpsertSession(db.Session{
		ID: "preserved-ssh-session", Project: "archive", Machine: "ssh-box",
		Agent: "claude", FilePath: &missingPath, MessageCount: 1,
	}))
	sshCalls := 0
	stubRunRemoteSync(t, func(
		_ context.Context, rs *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		sshCalls++
		assert.True(t, rs.Full)
		return ssh.SyncStats{}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{{
				Host: "ssh-box", Transport: config.RemoteTransportSSH,
			}},
		}, nil,
	)

	require.NotNil(t, response.LocalStats)
	assert.Empty(t, response.Error)
	assert.Empty(t, response.Failures)
	assert.Equal(t, 1, sshCalls,
		"SSH import must run after the legacy local fallback")
	preserved, err := f.db.GetSession(context.Background(), "preserved-ssh-session")
	require.NoError(t, err)
	assert.NotNil(t, preserved)
}

func TestRunRemoteSyncRequestRemoteOnlyKeepsActiveHTTPPath(t *testing.T) {
	f := newSyncRouteFixture(t)
	prepareCalls := 0
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		prepareCalls++
		return &fakePreparedHTTPRebuild{}, nil
	})
	activeCalls := 0
	stubRunHTTPRemoteSync(t, func(
		_ context.Context, rh config.RemoteHost, full bool,
	) (remotesync.SyncStats, error) {
		activeCalls++
		assert.Equal(t, "alpha", rh.Host)
		assert.True(t, full)
		return remotesync.SyncStats{}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{Full: true, Hosts: []config.RemoteHost{{
			Host: "alpha", Transport: config.RemoteTransportHTTP,
		}}}, nil,
	)

	assert.Empty(t, response.Failures)
	assert.Zero(t, prepareCalls)
	assert.Equal(t, 1, activeCalls)
}

func TestRunRemoteSyncRequestIncrementalKeepsActiveHTTPPath(t *testing.T) {
	f := newSyncRouteFixture(t)
	prepareCalls := 0
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		prepareCalls++
		return &fakePreparedHTTPRebuild{}, nil
	})
	activeCalls := 0
	stubRunHTTPRemoteSync(t, func(
		_ context.Context, _ config.RemoteHost, full bool,
	) (remotesync.SyncStats, error) {
		activeCalls++
		assert.False(t, full)
		return remotesync.SyncStats{}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{IncludeLocal: true, Hosts: []config.RemoteHost{{
			Host: "alpha", Transport: config.RemoteTransportHTTP,
		}}}, nil,
	)

	assert.Empty(t, response.Failures)
	assert.Zero(t, prepareCalls)
	assert.Equal(t, 1, activeCalls)
}

func TestRunRemoteSyncRequestAttributesOuterOwnedHTTPCleanup(t *testing.T) {
	for _, includeLocal := range []bool{false, true} {
		name := "remote-only"
		if includeLocal {
			name = "include-local"
		}
		t.Run(name, func(t *testing.T) {
			f := newSyncRouteFixture(t)
			owner := &serverHTTPCleanupError{
				cause: errors.New("active HTTP import failed"),
				results: []error{
					errors.New("cleanup still holds mirror"),
					nil,
				},
			}
			stubRunHTTPRemoteSync(t, func(
				_ context.Context, rh config.RemoteHost, _ bool,
			) (remotesync.SyncStats, error) {
				assert.Equal(t, "alpha", rh.Host)
				return remotesync.SyncStats{}, owner
			})

			response := f.srv.runRemoteSyncRequest(
				context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
				remoteSyncRequest{
					IncludeLocal: includeLocal,
					Hosts: []config.RemoteHost{{
						Host: "alpha", Transport: config.RemoteTransportHTTP,
					}},
				}, nil,
			)

			assert.Empty(t, response.Error,
				"an HTTP host cleanup failure is not a local sync failure")
			require.Len(t, response.Failures, 1,
				"the outer coordinator reports the host exactly once")
			assert.Equal(t, "alpha", response.Failures[0].Host.Host)
			assert.Equal(t, "HTTP remote sync failed", response.Failures[0].Err)
			assert.Equal(t, 1, owner.retries)
		})
	}
}

func TestRunRemoteSyncRequestIncrementalRetainsActiveHTTPCleanup(t *testing.T) {
	f := newSyncRouteFixture(t)
	owner := &serverHTTPCleanupError{
		cause: errors.New("active HTTP import failed"),
		results: []error{
			errors.New("cleanup still holds mirror"),
			nil,
		},
	}
	var callbacks []string
	stubRunHTTPRemoteSync(t, func(
		_ context.Context, rh config.RemoteHost, _ bool,
	) (remotesync.SyncStats, error) {
		callbacks = append(callbacks, rh.Host)
		if rh.Host == "alpha" {
			return remotesync.SyncStats{}, owner
		}
		return remotesync.SyncStats{SessionsSynced: 1}, nil
	})
	httpHost := func(host string) config.RemoteHost {
		return config.RemoteHost{Host: host, Transport: config.RemoteTransportHTTP}
	}

	first := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			IncludeLocal: true, Hosts: []config.RemoteHost{httpHost("alpha")},
		}, nil,
	)
	require.Len(t, first.Failures, 1)
	assert.Equal(t, []string{"alpha"}, callbacks)
	assert.Equal(t, 1, owner.retries)

	second := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			IncludeLocal: true, Hosts: []config.RemoteHost{httpHost("beta")},
		}, nil,
	)
	assert.Empty(t, second.Failures)
	assert.Equal(t, []string{"alpha", "beta"}, callbacks)
	assert.Equal(t, 2, owner.retries)
}

func TestRunRemoteSyncRequestAutomaticResyncUsesUnifiedHTTPPath(t *testing.T) {
	f := newSyncRouteFixture(t, withStaleDB())
	f.writeClaudeSession(t, "proj/local.jsonl", "automatic unified rebuild")
	prepareCalls := 0
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		prepareCalls++
		return &fakePreparedHTTPRebuild{}, nil
	})
	activeHTTPCalls := 0
	stubRunHTTPRemoteSync(t, func(
		context.Context, config.RemoteHost, bool,
	) (remotesync.SyncStats, error) {
		activeHTTPCalls++
		return remotesync.SyncStats{}, nil
	})
	sshCalls := 0
	stubRunRemoteSync(t, func(
		_ context.Context, rs *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		sshCalls++
		assert.True(t, rs.Full)
		assert.False(t, f.db.NeedsResync(),
			"post-rebuild SSH must observe the swapped data version")
		return ssh.SyncStats{}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		context.Background(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
				{Host: "beta", Transport: config.RemoteTransportSSH},
			},
		}, nil,
	)

	assert.Empty(t, response.Failures)
	assert.Equal(t, 1, prepareCalls)
	assert.Zero(t, activeHTTPCalls)
	assert.Equal(t, 1, sshCalls)
	assert.False(t, f.db.NeedsResync())
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

	failures, stats, blocked := f.srv.runRemoteSyncHosts(
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
	require.NoError(t, blocked)
	assert.False(t, sshCalled, "server HTTP remote must not use SSH runner")
	assert.Equal(t, "https://alpha.example.test", got.URL)
	assert.Equal(t, remotesync.SyncStats{SessionsSynced: 1, SessionsTotal: 1}, stats)
}

func TestRunRemoteSyncHostsRetainsFailedHTTPCleanupUntilReleased(t *testing.T) {
	f := newSyncRouteFixture(t)
	owner := &serverHTTPCleanupError{
		cause: errors.New("alpha HTTP sync failed"),
		results: []error{
			errors.New("cleanup failed after alpha"),
			errors.New("cleanup still blocks beta"),
			errors.New("cleanup still blocks later request"),
			nil,
		},
	}
	var callbacks []string
	stubRunHTTPRemoteSync(t, func(
		_ context.Context, rh config.RemoteHost, _ bool,
	) (remotesync.SyncStats, error) {
		callbacks = append(callbacks, rh.Host)
		if rh.Host == "alpha" {
			return remotesync.SyncStats{}, owner
		}
		return remotesync.SyncStats{SessionsSynced: 1}, nil
	})
	httpHost := func(host string) config.RemoteHost {
		return config.RemoteHost{Host: host, Transport: config.RemoteTransportHTTP}
	}

	failures, _, blocked := f.srv.runRemoteSyncHosts(
		context.Background(), f.db, []config.RemoteHost{
			httpHost("alpha"), httpHost("beta"), httpHost("gamma"),
		}, false, nil,
	)
	require.Len(t, failures, 1)
	assert.Equal(t, "alpha", failures[0].Host.Host)
	var pending *remotesync.PendingCleanupError
	require.ErrorAs(t, blocked, &pending)
	assert.ErrorIs(t, blocked, owner)
	assert.Equal(t, []string{"alpha"}, callbacks,
		"beta's callback is blocked and iteration stops before gamma")
	assert.Equal(t, 2, owner.retries)

	failures, _, blocked = f.srv.runRemoteSyncHosts(
		context.Background(), f.db,
		[]config.RemoteHost{httpHost("delta")}, false, nil,
	)
	assert.Empty(t, failures)
	require.ErrorAs(t, blocked, &pending)
	assert.Equal(t, []string{"alpha"}, callbacks)
	assert.Equal(t, 3, owner.retries)

	failures, stats, blocked := f.srv.runRemoteSyncHosts(
		context.Background(), f.db,
		[]config.RemoteHost{httpHost("epsilon")}, false, nil,
	)
	assert.Empty(t, failures)
	require.NoError(t, blocked)
	assert.Equal(t, 1, stats.SessionsSynced)
	assert.Equal(t, []string{"alpha", "epsilon"}, callbacks)
	assert.Equal(t, 4, owner.retries)
}

type serverHTTPCleanupError struct {
	cause   error
	results []error
	retries int
}

func (e *serverHTTPCleanupError) Error() string { return e.cause.Error() }

func (e *serverHTTPCleanupError) Unwrap() error { return e.cause }

func (e *serverHTTPCleanupError) RetryCleanup() error {
	result := e.results[e.retries]
	e.retries++
	return result
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
