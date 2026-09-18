package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	stdlibsync "sync"
	"syscall"
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

type offlineRemoteTransport struct{}

func (offlineRemoteTransport) RoundTrip(
	*http.Request,
) (*http.Response, error) {
	return nil, syscall.ETIMEDOUT
}

type syncRouteFixtureConfig struct {
	stale          bool
	archiveContent config.ArchiveContent
	remoteHosts    []config.RemoteHost
	disabledAgents []parser.AgentType
	extraAgentDirs map[parser.AgentType][]string
	broadcaster    *Broadcaster
	engine         *syncpkg.Engine
	syncRunner     LocalSyncRunner
	resyncRunner   LocalResyncRunner
}

type syncRouteFixtureOption func(*syncRouteFixtureConfig)

func captureServerLogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}

func withStaleDB() syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.stale = true }
}

func withUsageOnlyStorage() syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) {
		c.archiveContent = config.ArchiveContentUsage
	}
}

func withLocalSyncRunner(r LocalSyncRunner) syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.syncRunner = r }
}

func withLocalResyncRunner(r LocalResyncRunner) syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.resyncRunner = r }
}

func withRemoteHosts(hosts ...config.RemoteHost) syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) { c.remoteHosts = hosts }
}

func withDisabledAgents(
	disabled []parser.AgentType,
	dirs map[parser.AgentType][]string,
) syncRouteFixtureOption {
	return func(c *syncRouteFixtureConfig) {
		c.disabledAgents = append([]parser.AgentType(nil), disabled...)
		c.extraAgentDirs = dirs
	}
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
		Host:           "127.0.0.1",
		Port:           0,
		DataDir:        dir,
		DBPath:         dbPath,
		WriteTimeout:   30 * time.Second,
		ArchiveContent: cfg.archiveContent,
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
		RemoteHosts:    cfg.remoteHosts,
		DisabledAgents: cfg.disabledAgents,
	}
	for agent, dirs := range cfg.extraAgentDirs {
		serverConfig.AgentDirs[agent] = append([]string(nil), dirs...)
	}
	var serverOptions []Option
	if cfg.broadcaster != nil {
		serverOptions = append(serverOptions, WithBroadcaster(cfg.broadcaster))
	}
	if cfg.syncRunner != nil {
		serverOptions = append(serverOptions, WithLocalSyncRunner(cfg.syncRunner))
	}
	if cfg.resyncRunner != nil {
		serverOptions = append(serverOptions,
			WithLocalResyncRunner(cfg.resyncRunner))
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
	_, err = raw.ExecContext(t.Context(), "PRAGMA user_version = 0")
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
	req := httptest.NewRequestWithContext(t.Context(), method, path, reader)
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
	page, err := database.ListSessions(t.Context(), db.SessionFilter{
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assertFirstMessageContains(t, page.Sessions[0].FirstMessage, want)
}

func assertSessionCount(t *testing.T, database *db.DB, want int) {
	t.Helper()
	page, err := database.ListSessions(t.Context(), db.SessionFilter{
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
	options     syncpkg.RebuildOptions
	closed      int
	committed   int
	closeErrors []error
}

func (p *fakePreparedHTTPRebuild) BorrowRebuildOptions() (
	syncpkg.RebuildOptions, func(), error,
) {
	return p.options, func() {}, nil
}

func (p *fakePreparedHTTPRebuild) Close() error {
	p.closed++
	if p.closed <= len(p.closeErrors) {
		return p.closeErrors[p.closed-1]
	}
	return nil
}

func (p *fakePreparedHTTPRebuild) Commit() error {
	p.committed++
	return nil
}

func TestPreparedHTTPRebuildLeaseForwardsCommitOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	prepared := &fakePreparedHTTPRebuild{}
	lease := &preparedHTTPRebuildLease{prepared: prepared, release: func() {}}
	require.NoError(lease.Commit())
	require.NoError(lease.Commit())
	assert.Equal(1, prepared.committed)
	assert.Zero(prepared.closed, "commit must not infer cleanup")
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
	assert := assert.New(t)
	require := require.New(t)

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
	require.Error(seedErr)

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
			t.Context(), f.db, engine,
			remoteSyncRequest{Hosts: []config.RemoteHost{host("remote-only")}}, nil,
		)
	}()

	select {
	case <-owner.probeEntered:
	case <-time.After(time.Second):
		require.FailNow("cleanup retry could not acquire engine")
	}

	includeLocalDone := make(chan remoteSyncResponse, 1)
	go func() {
		includeLocalDone <- f.srv.runRemoteSyncRequest(
			t.Context(), f.db, engine,
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
			assert.Empty(response.Failures)
		case <-time.After(time.Second):
			require.FailNow("HTTP request did not complete")
		}
	}
	mu.Lock()
	assert.Equal([]string{"remote-only", "include-local"}, callbacks)
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
	assert := assert.New(t)
	require := require.New(t)

	f := newSyncRouteFixture(t)
	engine := f.srv.syncEngineForLocal(f.db)
	cleanup := &failingRebuildCleanup{errors: []error{
		errors.New("deferred close failed"),
		errors.New("immediate retry failed"),
	}}
	registry := remotesync.CleanupRegistry{}

	_, firstErr := registry.Run(func() (remotesync.SyncStats, error) {
		_, err := engine.SyncThenRunWithRebuild(
			t.Context(), true, nil,
			func() (syncpkg.RebuildOptions, syncpkg.RebuildCleanup, error) {
				return syncpkg.RebuildOptions{}, cleanup, nil
			},
			nil,
			func(bool, bool) error { return nil },
		)
		return remotesync.SyncStats{}, err
	})
	require.Error(firstErr)
	assert.Equal(2, cleanup.calls,
		"registry must immediately retry the failed deferred close")

	nextRan := false
	_, secondErr := registry.Run(func() (remotesync.SyncStats, error) {
		nextRan = true
		return remotesync.SyncStats{}, nil
	})
	require.NoError(secondErr)
	assert.True(nextRan)
	assert.Equal(3, cleanup.calls,
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
	assert := assert.New(t)

	f := newSyncRouteFixture(t)
	f.writeClaudeSession(t, "proj/local.jsonl", "local survives contributor failure")
	assertSessionCount(t, f.db, 0)

	sentinel := errors.New("persist remote cache")
	prepared := &fakePreparedHTTPRebuild{options: syncpkg.RebuildOptions{
		Contributors: []syncpkg.RebuildContributor{{
			Name:      "alpha",
			AfterSync: func(*syncpkg.Engine, *db.DB) error { return sentinel },
		}},
	}}
	stubPrepareHTTPRebuild(t, func(
		_ context.Context, syncs []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		require.Len(t, syncs, 1)
		assert.Equal("alpha", syncs[0].Host)
		assert.Equal(remotesync.FullImportExplicit, syncs[0].FullReason)
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
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
				{Host: "beta", Transport: config.RemoteTransportSSH},
			},
		}, nil,
	)

	require.Len(t, response.Failures, 1)
	assert.Equal("alpha", response.Failures[0].Host.Host)
	assert.NotContains(response.Failures[0].Err, sentinel.Error())
	assert.Zero(sshCalls)
	assert.Equal(1, prepared.closed)
	assertSessionCount(t, f.db, 0)
}

func TestRunRemoteSyncRequestContributorFailurePrecedesRetainedCleanupHost(t *testing.T) {
	assert := assert.New(t)

	f := newSyncRouteFixture(t)
	contributorCause := &remotesync.StatusError{
		Code: http.StatusForbidden, Detail: "private contributor detail",
	}
	cleanupCause := errors.New("beta mirror cleanup failed")
	cleanupErr := &remotesync.HostError{
		Host: "beta", Operation: "cleanup", Err: cleanupCause,
	}
	prepared := &fakePreparedHTTPRebuild{
		options: syncpkg.RebuildOptions{Contributors: []syncpkg.RebuildContributor{{
			Name: "alpha",
			AfterSync: func(*syncpkg.Engine, *db.DB) error {
				return contributorCause
			},
		}}},
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
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{httpHost("alpha"), httpHost("beta")},
		}, nil,
	)
	require.Len(t, first.Failures, 1)
	assert.Equal("alpha", first.Failures[0].Host.Host)
	assert.Contains(first.Failures[0].Err, "403 Forbidden")
	assert.NotContains(first.Failures[0].Err, contributorCause.Detail)
	assert.Equal(2, prepared.closed,
		"failed deferred cleanup must be retried and retained")
	assert.Zero(activeCalls)

	second := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{Hosts: []config.RemoteHost{httpHost("gamma")}}, nil,
	)
	assert.Empty(second.Failures)
	assert.Equal(3, prepared.closed,
		"next request must release retained beta cleanup first")
	assert.Equal(1, activeCalls)
}

func TestRunRemoteSyncRequestUnifiedHTTPUsesMirrorDeltaAndBulkRebuild(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	broadcaster := NewBroadcaster(0)
	f := newSyncRouteFixture(t, withBroadcasterForSyncRoutes(broadcaster))
	logs := captureServerLogOutput(t)
	events, unsubscribe := broadcaster.Subscribe()
	t.Cleanup(unsubscribe)
	f.writeClaudeSession(t, "proj/local.jsonl", "unified local")
	remoteDir := filepath.Join(t.TempDir(), "remote-claude")
	remotePath := filepath.Join(remoteDir, "project", "remote.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(remotePath), 0o755))
	require.NoError(os.WriteFile(
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
		assert.Equal(strconv.Itoa(remotesync.ProtocolVersion),
			r.Header.Get(remotesync.ProtocolHeader))
		remotesync.SetProtocolHeader(w.Header())
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			if err := json.MarshalWrite(w, targets); err != nil {
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
			if err := json.MarshalWrite(w, manifest); err != nil {
				serverErrors <- err
			}
		case "/api/v1/remote-sync/archive":
			var request remotesync.ArchiveRequest
			if err := json.UnmarshalRead(r.Body, &request); err != nil {
				serverErrors <- err
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			archiveRequests++
			w.Header().Set("Content-Type", "application/x-tar")
			var err error
			if request.DeltaFiles == nil {
				err = remotesync.WriteArchive(r.Context(), w, request.TargetSet)
			} else {
				err = remotesync.WriteArchiveFiles(
					w, targets, request.DeltaFiles,
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
	hostAlpha := config.RemoteHost{
		Host: "alpha", Transport: config.RemoteTransportHTTP,
		URL: ts.URL, Token: "remote-token",
	}
	hostGamma := config.RemoteHost{
		Host: "gamma", Transport: config.RemoteTransportHTTP,
		URL: ts.URL, Token: "remote-token",
	}
	sshCalls := 0
	stubRunRemoteSync(t, func(
		_ context.Context, rs *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		assert.Equal("beta", rs.Host)
		sshCalls++
		return ssh.SyncStats{}, nil
	})

	for range 2 {
		response := f.srv.runRemoteSyncRequest(
			t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
			remoteSyncRequest{
				Full: true, IncludeLocal: true,
				Hosts: []config.RemoteHost{
					hostGamma,
					{Host: "beta", Transport: config.RemoteTransportSSH},
					hostAlpha,
				},
			}, nil,
		)
		assert.Empty(response.Failures)
		require.NotNil(response.LocalStats)
		assert.False(response.LocalStats.Aborted)
		assert.Equal(3, response.LocalStats.Synced)
		select {
		case event := <-events:
			assert.Equal("sync", event.Scope)
		case <-time.After(time.Second):
			require.FailNow("unified rebuild did not emit a sync event")
		}
	}

	assert.Equal(2, archiveRequests,
		"unchanged full rebuild should reuse the prepared mirror")
	assert.Equal(2, sshCalls)
	assertSessionCount(t, f.db, 3)
	output := logs.String()
	assert.Contains(output,
		"remote sync HTTP contributors started: hosts=2")
	assert.Contains(output,
		"remote sync HTTP contributors finished: hosts=2")
	for _, host := range []string{"alpha", "gamma"} {
		assert.Equal(2, strings.Count(output,
			"remote sync HTTP host preparation started: host="+host))
		assert.Equal(2, strings.Count(output,
			"remote sync HTTP host preparation finished: host="+host))
		assert.Regexp("remote sync HTTP host preparation finished: host="+host+
			`[^\n]*duration=[^\n]*outcome=completed`,
			output,
		)
		assert.Equal(2, strings.Count(output,
			"remote sync host started: host="+host+
				" transport=http full=true mode=unified_rebuild"))
		assert.Equal(2, strings.Count(output,
			"remote sync host finished: host="+host+" transport=http"))
		assert.Regexp("remote sync host finished: host="+host+
			` transport=http[^\n]*sessions_synced=1`+
			`[^\n]*sessions_total=1[^\n]*outcome=completed`,
			output,
		)
	}
	httpFinished := strings.Index(output,
		"remote sync HTTP contributors finished: hosts=2")
	sshStarted := strings.Index(output,
		"remote sync host started: host=beta transport=ssh")
	require.NotEqual(-1, httpFinished)
	require.NotEqual(-1, sshStarted)
	for _, host := range []string{"alpha", "gamma"} {
		hostFinished := strings.Index(output,
			"remote sync host finished: host="+host+" transport=http")
		require.NotEqual(-1, hostFinished)
		assert.Less(hostFinished, sshStarted,
			"each HTTP contributor must finish before post-rebuild SSH work")
	}
	assert.Less(httpFinished, sshStarted,
		"HTTP contributor completion must precede post-rebuild SSH work")
	assert.Contains(output, "aggregate_synced=3")
	assert.NotContains(output, "local_synced=")
	select {
	case err := <-serverErrors:
		require.NoError(err)
	default:
	}
}

func TestRunRemoteSyncRequestHTTPPreparationFailureSkipsSSHAndSwap(t *testing.T) {
	assert := assert.New(t)

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
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
				{Host: "beta", Transport: config.RemoteTransportSSH},
			},
		}, nil,
	)

	require.Len(t, response.Failures, 1)
	assert.Equal("alpha", response.Failures[0].Host.Host)
	assert.Contains(response.Failures[0].Err, "403 Forbidden")
	assert.NotContains(response.Failures[0].Err, remoteCause.Detail)
	assert.Zero(sshCalls)
	assert.Equal(1, prepared.closed)
	assertSessionCount(t, f.db, 0)
}

func TestRunRemoteSyncRequestAbortedUnifiedRebuildReturnsTopLevelError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newSyncRouteFixture(t)
	missingPath := filepath.Join(f.dir, "missing.jsonl")
	require.NoError(f.db.UpsertSession(db.Session{
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
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
				{Host: "beta", Transport: config.RemoteTransportSSH},
			},
		}, nil,
	)

	require.NotNil(response.LocalStats)
	assert.True(response.LocalStats.Aborted)
	assert.Equal("unified local and HTTP rebuild aborted", response.Error)
	assert.Empty(response.Failures,
		"an aggregate rebuild abort is not a host failure")
	assert.Zero(sshCalls)
	preserved, err := f.db.GetSession(t.Context(), "preserved-old-session")
	require.NoError(err)
	assert.NotNil(preserved)
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
	assert := assert.New(t)

	f := newSyncRouteFixture(t)
	logs := captureServerLogOutput(t)
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		return &fakePreparedHTTPRebuild{}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
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
	assert.True(response.LocalStats.Aborted)
	assert.Equal(context.Canceled.Error(), response.Error)
	assert.NotEqual(syncpkg.ErrUnifiedRebuildAborted.Error(), response.Error)
	assert.Empty(response.Failures)
	output := logs.String()
	assert.Contains(output,
		"remote sync request started: include_local=true full=true hosts=1")
	assert.Contains(output, "remote sync request finished: include_local=true full=true")
	assert.Contains(output, "duration=")
	assert.Contains(output, "outcome=canceled")
	assert.Contains(output, "error=\"context canceled\"")
	assert.NotContains(output, "secret")
}

func TestRunRemoteSyncHostsOwnedLogsPerHostLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newSyncRouteFixture(t)
	logs := captureServerLogOutput(t)
	privateURL := "http://example.invalid/private/archive?token=secret-token"
	stubRunHTTPRemoteSync(t, func(
		_ context.Context, rh config.RemoteHost, _ bool,
	) (remotesync.SyncStats, error) {
		if rh.Host == "alpha" {
			return remotesync.SyncStats{
				SessionsSynced: 3, SessionsTotal: 5, Skipped: 1,
			}, nil
		}
		return remotesync.SyncStats{}, &url.Error{
			Op: "Get", URL: privateURL, Err: errors.New("private response"),
		}
	})

	failures, totals, err := f.srv.runRemoteSyncHostsOwned(
		t.Context(), f.db,
		[]config.RemoteHost{
			{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret-token"},
			{Host: "beta", Transport: config.RemoteTransportHTTP, Token: "secret-token"},
		}, true, nil, true, false,
	)

	require.NoError(err)
	require.Len(failures, 1)
	assert.Equal(3, totals.SessionsSynced)
	assert.Equal(5, totals.SessionsTotal)
	assert.Equal(1, totals.Skipped)
	output := logs.String()
	assert.Contains(output,
		"remote sync host started: host=alpha transport=http full=true")
	assert.Contains(output,
		"remote sync host finished: host=alpha transport=http")
	assert.Contains(output, "sessions_synced=3")
	assert.Contains(output, "outcome=completed")
	assert.Contains(output,
		"remote sync host started: host=beta transport=http full=true")
	assert.Contains(output,
		"remote sync host finished: host=beta transport=http")
	assert.Contains(output, "outcome=failed")
	assert.Contains(output, "error=\"HTTP remote sync failed\"")
	assert.NotContains(output, privateURL)
	assert.NotContains(output, "secret-token")
}

func TestRunRemoteSyncRequestLogsRemoteOnlyAggregateStats(t *testing.T) {
	f := newSyncRouteFixture(t)
	logs := captureServerLogOutput(t)
	stubRunHTTPRemoteSync(t, func(
		_ context.Context, _ config.RemoteHost, _ bool,
	) (remotesync.SyncStats, error) {
		return remotesync.SyncStats{
			SessionsSynced: 3, SessionsTotal: 5, Skipped: 1, Failed: 1,
		}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Hosts: []config.RemoteHost{{
				Host: "alpha", Transport: config.RemoteTransportHTTP,
			}},
		}, nil,
	)

	require.Empty(t, response.Error)
	output := logs.String()
	assert.Contains(t, output,
		"aggregate_synced=3 aggregate_total=5 aggregate_skipped=1 aggregate_failed=1")
}

func TestRunRemoteSyncRequestCombinesMixedTransportAggregateStats(t *testing.T) {
	f := newSyncRouteFixture(t)
	logs := captureServerLogOutput(t)
	stubRunHTTPRemoteSync(t, func(
		_ context.Context, _ config.RemoteHost, _ bool,
	) (remotesync.SyncStats, error) {
		return remotesync.SyncStats{
			SessionsSynced: 2, SessionsTotal: 3, Skipped: 1,
		}, nil
	})
	stubRunRemoteSync(t, func(
		_ context.Context, _ *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		return ssh.SyncStats{
			SessionsSynced: 4, SessionsTotal: 5, Skipped: 2, Failed: 1,
		}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP},
				{Host: "beta", Transport: config.RemoteTransportSSH},
			},
		}, nil,
	)

	require.Empty(t, response.Error)
	output := logs.String()
	assert.Contains(t, output,
		"aggregate_synced=6 aggregate_total=8 aggregate_skipped=3 aggregate_failed=1")
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
			assert := assert.New(t)

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
				t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
				remoteSyncRequest{
					Full: true, IncludeLocal: true,
					Hosts: []config.RemoteHost{{
						Host: "alpha", Transport: config.RemoteTransportHTTP,
						Token: "configured-token",
					}},
				}, nil,
			)

			assert.Equal(tt.cause.Error(), response.Error)
			assert.NotContains(response.Error, privateURL)
			assert.NotContains(response.Error, "secret-token")
			assert.NotContains(response.Error, "/private/archive")
			assert.Empty(response.Failures)
		})
	}
}

func TestRunRemoteSyncRequestMixedRunsSSHFullAfterUnifiedSwap(t *testing.T) {
	f := newSyncRouteFixture(t)
	f.writeClaudeSession(t, "proj/local.jsonl", "local swapped before ssh")
	order := make([]string, 0, 3)
	prepared := &fakePreparedHTTPRebuild{options: syncpkg.RebuildOptions{
		Contributors: []syncpkg.RebuildContributor{{
			Name: "alpha",
			AfterSync: func(_ *syncpkg.Engine, database *db.DB) error {
				order = append(order, "http")
				return nil
			},
		}},
	}}
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
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
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
	assert := assert.New(t)
	require := require.New(t)

	f := newSyncRouteFixture(t)
	missingPath := filepath.Join(f.dir, "missing-remote.jsonl")
	require.NoError(f.db.UpsertSession(db.Session{
		ID: "preserved-ssh-session", Project: "archive", Machine: "ssh-box",
		Agent: "claude", FilePath: &missingPath, MessageCount: 1,
	}))
	sshCalls := 0
	stubRunRemoteSync(t, func(
		_ context.Context, rs *ssh.RemoteSync,
	) (ssh.SyncStats, error) {
		sshCalls++
		assert.True(rs.Full)
		return ssh.SyncStats{}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{{
				Host: "ssh-box", Transport: config.RemoteTransportSSH,
			}},
		}, nil,
	)

	require.NotNil(response.LocalStats)
	assert.Empty(response.Error)
	assert.Empty(response.Failures)
	assert.Equal(1, sshCalls,
		"SSH import must run after the legacy local fallback")
	preserved, err := f.db.GetSession(t.Context(), "preserved-ssh-session")
	require.NoError(err)
	assert.NotNil(preserved)
}

func TestRunRemoteSyncRequestRemoteOnlyKeepsActiveHTTPPath(t *testing.T) {
	f := newSyncRouteFixture(t)
	prepareCalls := 0
	stubPrepareHTTPRebuild(t, func(
		_ context.Context, syncs []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		require.Len(t, syncs, 1)
		assert.Equal(t, remotesync.FullImportDataRebuild, syncs[0].FullReason)
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
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{Full: true, Hosts: []config.RemoteHost{{
			Host: "alpha", Transport: config.RemoteTransportHTTP,
		}}}, nil,
	)

	assert.Empty(t, response.Failures)
	assert.Zero(t, prepareCalls)
	assert.Equal(t, 1, activeCalls)
}

func TestPrepareHTTPRebuildOmitsOfflineHost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var progress []syncpkg.Progress
	prepared, err := prepareHTTPRebuild(
		t.Context(), []remotesync.HTTPSync{{
			Host: "offline",
			URL:  "http://offline.invalid",
			Client: &http.Client{
				Transport: offlineRemoteTransport{},
			},
			Progress: func(p syncpkg.Progress) {
				progress = append(progress, p)
			},
		}},
	)

	require.NoError(err)
	require.NotNil(prepared)
	options, release, err := prepared.BorrowRebuildOptions()
	require.NoError(err)
	assert.Empty(options.Contributors)
	assert.Equal([]string{"offline~"},
		options.UnavailableContributorIDPrefixes)
	release()
	require.NoError(prepared.Close())
	assert.Contains(progress, syncpkg.Progress{
		Detail: "Skipped offline remote host offline",
	})
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
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{IncludeLocal: true, Hosts: []config.RemoteHost{{
			Host: "alpha", Transport: config.RemoteTransportHTTP,
		}}}, nil,
	)

	assert.Empty(t, response.Failures)
	assert.Zero(t, prepareCalls)
	assert.Equal(t, 1, activeCalls)
}

func TestRunRemoteSyncRequestConfiguredSkipsOfflineHTTPHost(t *testing.T) {
	assert := assert.New(t)

	f := newSyncRouteFixture(t)
	var calls []string
	stubRunHTTPRemoteSync(t, func(
		_ context.Context, rh config.RemoteHost, _ bool,
	) (remotesync.SyncStats, error) {
		calls = append(calls, rh.Host)
		if rh.Host == "offline" {
			return remotesync.SyncStats{}, syscall.ETIMEDOUT
		}
		return remotesync.SyncStats{SessionsSynced: 1}, nil
	})
	var progress []syncpkg.Progress

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "offline", Transport: config.RemoteTransportHTTP},
				{Host: "reachable", Transport: config.RemoteTransportHTTP},
			},
		},
		func(p syncpkg.Progress) { progress = append(progress, p) },
	)

	require.NotNil(t, response.LocalStats)
	assert.Empty(response.Error)
	assert.Empty(response.Failures)
	assert.Equal([]string{"offline", "reachable"}, calls)
	assert.Contains(progress, syncpkg.Progress{
		Detail: "Skipped offline remote host offline",
	})
}

func TestRunRemoteSyncRequestExplicitHostKeepsOfflineFailure(t *testing.T) {
	assert := assert.New(t)

	f := newSyncRouteFixture(t)
	stubRunHTTPRemoteSync(t, func(
		context.Context, config.RemoteHost, bool,
	) (remotesync.SyncStats, error) {
		return remotesync.SyncStats{}, syscall.ETIMEDOUT
	})

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{Hosts: []config.RemoteHost{{
			Host: "offline", Transport: config.RemoteTransportHTTP,
		}}}, nil,
	)

	assert.Empty(response.Error)
	require.Len(t, response.Failures, 1)
	assert.Equal("offline", response.Failures[0].Host.Host)
	assert.Contains(response.Failures[0].Err, "connection timed out")
}

func TestRunRemoteSyncRequestAttributesOuterOwnedHTTPCleanup(t *testing.T) {
	for _, includeLocal := range []bool{false, true} {
		name := "remote-only"
		if includeLocal {
			name = "include-local"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)

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
				assert.Equal("alpha", rh.Host)
				return remotesync.SyncStats{}, owner
			})

			response := f.srv.runRemoteSyncRequest(
				t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
				remoteSyncRequest{
					IncludeLocal: includeLocal,
					Hosts: []config.RemoteHost{{
						Host: "alpha", Transport: config.RemoteTransportHTTP,
					}},
				}, nil,
			)

			assert.Empty(response.Error,
				"an HTTP host cleanup failure is not a local sync failure")
			require.Len(t, response.Failures, 1,
				"the outer coordinator reports the host exactly once")
			assert.Equal("alpha", response.Failures[0].Host.Host)
			assert.Equal("HTTP remote sync failed", response.Failures[0].Err)
			assert.Equal(1, owner.retries)
		})
	}
}

func TestRunRemoteSyncRequestIncrementalRetainsActiveHTTPCleanup(t *testing.T) {
	assert := assert.New(t)

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
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			IncludeLocal: true, Hosts: []config.RemoteHost{httpHost("alpha")},
		}, nil,
	)
	require.Len(t, first.Failures, 1)
	assert.Equal([]string{"alpha"}, callbacks)
	assert.Equal(1, owner.retries)

	second := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			IncludeLocal: true, Hosts: []config.RemoteHost{httpHost("beta")},
		}, nil,
	)
	assert.Empty(second.Failures)
	assert.Equal([]string{"alpha", "beta"}, callbacks)
	assert.Equal(2, owner.retries)
}

func TestRunRemoteSyncRequestAutomaticResyncUsesUnifiedHTTPPath(t *testing.T) {
	assert := assert.New(t)

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
		assert.True(rs.Full)
		assert.False(f.db.NeedsResync(),
			"post-rebuild SSH must observe the swapped data version")
		return ssh.SyncStats{}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(f.db),
		remoteSyncRequest{
			IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
				{Host: "beta", Transport: config.RemoteTransportSSH},
			},
		}, nil,
	)

	assert.Empty(response.Failures)
	assert.Equal(1, prepareCalls)
	assert.Zero(activeHTTPCalls)
	assert.Equal(1, sshCalls)
	assert.False(f.db.NeedsResync())
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

func TestArchiveMaintenanceNoSyncSharesBarrier(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Server, func() error) error
	}{
		{"foreground", (*Server).tryArchiveWrite},
		{"background", (*Server).serializeArchiveWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSyncRouteFixture(t)
			t.Cleanup(func() { require.NoError(t, f.srv.Shutdown(t.Context())) })
			started := make(chan struct{})
			unblock := make(chan struct{})
			release := stdlibsync.OnceFunc(func() { close(unblock) })
			done := make(chan error, 1)
			go func() {
				done <- tc.run(f.srv, func() error {
					close(started)
					<-unblock
					return nil
				})
			}()
			t.Cleanup(func() {
				release()
				require.NoError(t, <-done)
			})
			<-started

			for _, request := range []struct {
				path string
				body map[string]any
			}{
				{"/api/v1/data/compact", map[string]any{}},
				{"/api/v1/data/strip-images", map[string]any{"confirmed": true}},
			} {
				response := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					response <- serveJSON(t, f.handler, http.MethodPost, request.path, request.body)
				}()
				select {
				case w := <-response:
					assert.Equal(t, http.StatusConflict, w.Code, "%s: %s", request.path, w.Body.String())
				case <-time.After(5 * time.Second):
					release()
					<-response
					require.FailNow(t, request.path+" waited for maintenance instead of returning a conflict")
				}
			}
		})
	}
}

func TestSyncEngineForLocalCarriesUsageOnlyStoragePolicy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := newSyncRouteFixture(t, withUsageOnlyStorage())
	f.writeClaudeSession(t, "proj/private.jsonl", "private prompt")

	stats := f.srv.syncEngineForLocal(f.db).SyncAll(t.Context(), nil)
	require.Equal(1, stats.Synced)

	page, err := f.db.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(err)
	require.Len(page.Sessions, 1)
	assert.Nil(page.Sessions[0].FirstMessage)

	messages, err := f.db.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(err)
	assert.Empty(messages,
		"a session without usage or assistant activity needs no message rows")
	for _, message := range messages {
		assert.Empty(message.Content)
		assert.Empty(message.ThinkingText)
		assert.Empty(message.ToolCalls)
		assert.Empty(message.ToolResults)
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
	engine.SyncAll(t.Context(), nil)

	out, err := srv.humaSyncStatus(t.Context(), &emptyInput{})

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

// TestHumaTriggerSyncWorkerBackedRejectsStaleArchive pins the new UX: with the
// worker-backed runner wired, /sync on a stale archive returns 409 pointing at
// /resync and never runs the runner, since the worker refuses to swap the
// archive under the live daemon.
func TestHumaTriggerSyncWorkerBackedRejectsStaleArchive(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ran := false
	f := newSyncRouteFixture(t, withStaleDB(), withLocalSyncRunner(
		func(context.Context, func(syncpkg.Progress)) (syncpkg.SyncStats, error) {
			ran = true
			return syncpkg.SyncStats{}, nil
		},
	))
	require.True(f.db.NeedsResync())

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync", nil)

	require.Equal(http.StatusConflict, w.Code, "body: %s", w.Body.String())
	assert.Contains(w.Body.String(), "resync")
	assert.Equal("true", w.Header().Get(ResyncRequiredHeader),
		"the rejection must carry the machine-readable resync signal for the CLI")
	assert.False(ran, "the worker-backed runner must not run for a stale archive")
	assert.True(f.db.NeedsResync(), "a rejected sync must not resync")
}

// TestHumaTriggerSyncWorkerRunnerErrorRejectsStream pins the failure UX: a
// worker-backed runner that ran and reported failure must surface an SSE
// "error" event (or an error status without SSE) instead of a "done" event
// that makes the failed pass look successful.
func TestHumaTriggerSyncWorkerRunnerErrorRejectsStream(t *testing.T) {
	f := newSyncRouteFixture(t, withLocalSyncRunner(
		func(context.Context, func(syncpkg.Progress)) (syncpkg.SyncStats, error) {
			return syncpkg.SyncStats{}, errors.New("sync worker pass reported failed")
		},
	))

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync", nil)

	body := w.Body.String()
	assert.Contains(t, body, "event: error")
	assert.Contains(t, body, "sync worker pass reported failed")
	assert.NotContains(t, body, "event: done",
		"a failed worker pass must not be reported as a completed sync")
}

func TestHumaTriggerSyncDoesNotRetryFailures(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
		err  error
	}{
		{"busy without wait", "/api/v1/sync", syncpkg.ErrSyncInProgress},
		{"worker failed with wait", "/api/v1/sync?wait=true", errors.New("worker failed")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			f := newSyncRouteFixture(t, withLocalSyncRunner(
				func(context.Context, func(syncpkg.Progress)) (syncpkg.SyncStats, error) {
					calls++
					return syncpkg.SyncStats{}, tt.err
				},
			))
			w := serveJSON(t, f.handler, http.MethodPost, tt.path, nil)
			assert.Contains(t, w.Body.String(), "event: error")
			assert.Contains(t, w.Body.String(), tt.err.Error())
			assert.Equal(t, 1, calls)
		})
	}
}

func TestHumaTriggerResyncWorkerRunnerErrorRejectsStream(t *testing.T) {
	f := newSyncRouteFixture(t, withLocalResyncRunner(
		func(context.Context, func(syncpkg.Progress)) (syncpkg.SyncStats, error) {
			return syncpkg.SyncStats{}, errors.New("resync build reported failed")
		},
	))

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/resync", nil)

	body := w.Body.String()
	assert.Contains(t, body, "event: error")
	assert.Contains(t, body, "resync build reported failed")
	assert.NotContains(t, body, "event: done",
		"a failed worker resync must not be reported as a completed resync")
}

func TestForegroundSyncWorkerDeferredProcessingRejectsStream(t *testing.T) {
	f := newSyncRouteFixture(t, withLocalSyncRunner(func(
		context.Context, func(syncpkg.Progress),
	) (syncpkg.SyncStats, error) {
		return syncpkg.SyncStats{Deferred: 1}, nil
	}))

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync", nil)

	body := w.Body.String()
	assert.Contains(t, body, "event: error")
	assert.Contains(t, body, "local sync processing incomplete")
	assert.NotContains(t, body, "event: done")
}

func TestForegroundResyncWorkerDeferredProcessingRejectsStream(t *testing.T) {
	f := newSyncRouteFixture(t, withLocalResyncRunner(func(
		context.Context, func(syncpkg.Progress),
	) (syncpkg.SyncStats, error) {
		return syncpkg.SyncStats{Deferred: 1}, nil
	}))

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/resync", nil)

	body := w.Body.String()
	assert.Contains(t, body, "event: error")
	assert.Contains(t, body, "local sync processing incomplete")
	assert.NotContains(t, body, "event: done")
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
					t.Context(), engine, nil,
				)
			},
		},
		{
			name: "resync",
			run: func(srv *Server, engine *syncpkg.Engine) {
				srv.runResyncWithFallback(
					t.Context(), engine, nil,
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

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, err := f.srv.humaSyncSession(ctx, &sessionSyncInput{
		Body: service.SyncInput{Path: filepath.Join(f.dir, "missing.jsonl")},
	})

	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestHumaSyncSessionCanceledServiceSyncReturnsNil(t *testing.T) {
	f := newSyncRouteFixture(t)

	ctx, cancel := context.WithCancel(t.Context())
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
		t.Context(),
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
	assert := assert.New(t)

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
		t.Context(),
		f.db,
		[]config.RemoteHost{{
			Host:      "alpha",
			Transport: config.RemoteTransportHTTP,
			URL:       "https://alpha.example.test",
		}},
		false,
		nil,
	)

	assert.Empty(failures)
	require.NoError(t, blocked)
	assert.False(sshCalled, "server HTTP remote must not use SSH runner")
	assert.Equal("https://alpha.example.test", got.URL)
	assert.Equal(remotesync.SyncStats{SessionsSynced: 1, SessionsTotal: 1}, stats)
}

func TestRunRemoteSyncHostsRetainsFailedHTTPCleanupUntilReleased(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
		t.Context(), f.db, []config.RemoteHost{
			httpHost("alpha"), httpHost("beta"), httpHost("gamma"),
		}, false, nil,
	)
	require.Len(failures, 1)
	assert.Equal("alpha", failures[0].Host.Host)
	var pending *remotesync.PendingCleanupError
	require.ErrorAs(blocked, &pending)
	assert.ErrorIs(blocked, owner)
	assert.Equal([]string{"alpha"}, callbacks,
		"beta's callback is blocked and iteration stops before gamma")
	assert.Equal(2, owner.retries)

	failures, _, blocked = f.srv.runRemoteSyncHosts(
		t.Context(), f.db,
		[]config.RemoteHost{httpHost("delta")}, false, nil,
	)
	assert.Empty(failures)
	require.ErrorAs(blocked, &pending)
	assert.Equal([]string{"alpha"}, callbacks)
	assert.Equal(3, owner.retries)

	failures, stats, blocked := f.srv.runRemoteSyncHosts(
		t.Context(), f.db,
		[]config.RemoteHost{httpHost("epsilon")}, false, nil,
	)
	assert.Empty(failures)
	require.NoError(blocked)
	assert.Equal(1, stats.SessionsSynced)
	assert.Equal([]string{"alpha", "epsilon"}, callbacks)
	assert.Equal(4, owner.retries)
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
	assert := assert.New(t)

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
	assert.Contains(w.Header().Get("Content-Type"), "text/event-stream")
	body := w.Body.String()
	assert.Contains(body, "event: progress")
	assert.Contains(body, `"resync":true`)
	assert.Contains(body, "event: done")
	assert.Contains(body, `"local_stats"`)
}

func TestHumaSyncRemotesStreamsRemoteProgress(t *testing.T) {
	assert := assert.New(t)

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
	assert.Contains(w.Header().Get("Content-Type"), "text/event-stream")
	body := w.Body.String()
	assert.Contains(body, "event: progress")
	assert.Contains(body, "Resolving agent directories on alpha")
	assert.Contains(body, "event: done")
}

func TestRunRemoteSyncRequestSerializesNoSyncRemoteWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
			t.Context(),
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
		require.FailNow("remote sync did not enter")
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
		assert.Fail("exclusive operation overlapped remote sync")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRemote)

	select {
	case response := <-responseCh:
		assert.Empty(response.Failures)
	case <-time.After(time.Second):
		require.FailNow("remote sync did not finish")
	}
	select {
	case err := <-exclusiveErr:
		require.NoError(err)
	case <-time.After(time.Second):
		require.FailNow("exclusive operation did not finish")
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
	assert := assert.New(t)
	require := require.New(t)

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

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NotNil(got)
	assert.Equal(allowed.Host, got.Host)
	assert.Equal(allowed.User, got.User)
	assert.Equal(allowed.Port, got.Port)
}

func TestHumaSyncRemotesAllowsNonLocalConfiguredHostIgnoringInterval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NotNil(got)
	assert.Equal(requested.Host, got.Host)
	assert.Equal(requested.User, got.User)
	assert.Equal(requested.Port, got.Port)
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
	assert := assert.New(t)
	require := require.New(t)

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

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.NotContains(w.Body.String(), "stored.example-secret")
	assert.NotContains(w.Body.String(), "secret")
	assert.NotContains(w.Body.String(), "stored.example")
	resp := decodeRecorder[remoteSyncResponse](t, w)
	require.Len(resp.Failures, 1)
	assert.Equal(config.RemoteHost{Host: "devbox"}, resp.Failures[0].Host)
	assert.NotContains(resp.Failures[0].Err, "stored.example")
	assert.NotContains(resp.Failures[0].Err, "secret")
	assert.Equal("HTTP remote sync failed", resp.Failures[0].Err)
}

func TestSyncRemotesClassifiesHTTPFailures(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeRecorder[remoteSyncResponse](t, w)
	require.Len(resp.Failures, 1)
	assert.Contains(resp.Failures[0].Err,
		"rejected the sync token (401 Unauthorized)")
	assert.Contains(resp.Failures[0].Err,
		"must match the remote daemon's auth_token")
	assert.NotContains(resp.Failures[0].Err, "stored.example",
		"response body detail must not leak")
}

func TestRunHTTPRemoteSyncRequiresExplicitHTTPToken(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	t.Cleanup(ts.Close)

	_, err := runHTTPRemoteSync(
		t.Context(),
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
		assert.Equal(t, strconv.Itoa(remotesync.ProtocolVersion),
			r.Header.Get(remotesync.ProtocolHeader))
		remotesync.SetProtocolHeader(w.Header())
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
		t.Context(),
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

func TestRunHTTPRemoteSyncImportsLocallyDisabledProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	remoteRoot := t.TempDir()
	remoteSession := filepath.Join(
		remoteRoot, "tmp", "project", "chats", "session-remote.json",
	)
	require.NoError(os.MkdirAll(filepath.Dir(remoteSession), 0o755))
	require.NoError(os.WriteFile(remoteSession, []byte(testjsonl.GeminiSessionJSON(
		"remote-gemini", "project",
		"2026-08-09T10:00:00Z", "2026-08-09T10:01:00Z",
		[]map[string]any{testjsonl.GeminiUserMsg(
			"user", "2026-08-09T10:00:00Z", "import remote session",
		)},
	)), 0o644))
	targets := remotesync.TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentGemini: {remoteRoot},
	}}
	manifest, err := remotesync.BuildManifest(targets)
	require.NoError(err)
	serverErrors := make(chan error, 4)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remotesync.SetProtocolHeader(w.Header())
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			serverErrors <- json.MarshalWrite(w, targets)
		case "/api/v1/remote-sync/manifest":
			w.Header().Set("Content-Type", "application/json")
			serverErrors <- json.MarshalWrite(w, manifest)
		case "/api/v1/remote-sync/archive":
			w.Header().Set("Content-Type", "application/x-tar")
			serverErrors <- remotesync.WriteArchive(r.Context(), w, targets)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	database := dbtest.OpenTestDB(t)

	stats, err := runHTTPRemoteSync(
		t.Context(),
		config.Config{
			DataDir:        t.TempDir(),
			DisabledAgents: []parser.AgentType{parser.AgentGemini},
		},
		database,
		config.RemoteHost{
			Host: "devbox", Transport: config.RemoteTransportHTTP,
			URL: ts.URL, Token: "remote-token",
		},
		false,
		nil,
	)

	require.NoError(err)
	assert.Equal(1, stats.SessionsSynced)
	close(serverErrors)
	for serverErr := range serverErrors {
		require.NoError(serverErr)
	}
	stored, err := database.GetSession(
		t.Context(), "devbox~gemini:remote-gemini",
	)
	require.NoError(err)
	require.NotNil(stored)
	assert.Equal(string(parser.AgentGemini), stored.Agent)
}

func TestOnDemandSyncEngineExcludesDisabledProvider(t *testing.T) {
	geminiDir := filepath.Join(t.TempDir(), "gemini")
	f := newSyncRouteFixture(t, withDisabledAgents(
		[]parser.AgentType{parser.AgentGemini},
		map[parser.AgentType][]string{parser.AgentGemini: {geminiDir}},
	))

	engine := f.srv.syncEngineForLocal(f.db)

	assert.Empty(t, engine.ReconciliationRootsForAgent(string(parser.AgentGemini)))
	assert.Equal(t, []string{f.claudeDir},
		engine.ReconciliationRootsForAgent(string(parser.AgentClaude)))
}

func TestOnDemandSyncEngineKeepsStartupProvidersUntilRestart(t *testing.T) {
	geminiDir := filepath.Join(t.TempDir(), "gemini")
	f := newSyncRouteFixture(t, withDisabledAgents(nil,
		map[parser.AgentType][]string{parser.AgentGemini: {geminiDir}},
	))
	f.srv.mu.Lock()
	f.srv.cfg.DisabledAgents = []parser.AgentType{parser.AgentGemini}
	f.srv.mu.Unlock()

	engine := f.srv.syncEngineForLocal(f.db)

	assert.Equal(t, []string{geminiDir},
		engine.ReconciliationRootsForAgent(string(parser.AgentGemini)))
}
