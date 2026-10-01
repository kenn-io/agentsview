package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net"
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
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/service"
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

type offlineRemoteTransport struct{ err error }

func (tr offlineRemoteTransport) RoundTrip(
	*http.Request,
) (*http.Response, error) {
	if tr.err != nil {
		return nil, tr.err
	}
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
		database, err = db.Open(t.Context(), dbPath)
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
	serverOptions := []Option{
		WithReplicas(postgres.Backend{}, clickhouse.Backend{}), WithMirror(duckdb.Mirror{}),
	}
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

func (p *fakePreparedHTTPRebuild) BorrowRebuildOptions(ctx context.Context) (
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
	prepared := &fakePreparedHTTPRebuild{}
	lease := &preparedHTTPRebuildLease{prepared: prepared, release: func() {}}
	require.NoError(t, lease.Commit())
	require.NoError(t, lease.Commit())
	assert.Equal(t, 1, prepared.committed)
	assert.Zero(t, prepared.closed, "commit must not infer cleanup")
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

type lockOrderCleanupError struct {
	engine       *syncpkg.Engine
	probeEntered chan struct{}
	releaseProbe chan struct{}
	calls        int
}

func (c *lockOrderCleanupError) Error() string { return "pending cleanup" }

func (c *lockOrderCleanupError) RetryCleanup() error {
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
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	owner := &lockOrderCleanupError{
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
			t.Context(), f.db, engine,
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
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
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

func TestRunRemoteSyncRequestUnifiedHTTPContributorFailurePreservesArchive(t *testing.T) {
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
		assert.Equal(t, "alpha", syncs[0].Host)
		assert.Equal(t, remotesync.FullImportExplicit, syncs[0].FullReason)
		return prepared, nil
	})

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
			},
		}, nil,
	)

	require.Len(t, response.Failures, 1)
	assert.Equal(t, "alpha", response.Failures[0].Host.Host)
	assert.NotContains(t, response.Failures[0].Err, sentinel.Error())
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
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
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
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
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
	logs := captureServerLogOutput(t)
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
		assert.Equal(t, strconv.Itoa(remotesync.ProtocolVersion),
			r.Header.Get(remotesync.ProtocolHeader))
		remotesync.SetProtocolHeader(w.Header())
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			if err := json.MarshalWrite(w, targets); err != nil {
				serverErrors <- err
			}
		case "/api/v1/remote-sync/manifest":
			manifest, err := remotesync.BuildManifest(r.Context(), targets)
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
				err = remotesync.WriteArchiveFiles(r.Context(),
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

	for range 2 {
		response := f.srv.runRemoteSyncRequest(
			t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
			remoteSyncRequest{
				Full: true, IncludeLocal: true,
				Hosts: []config.RemoteHost{
					hostGamma,
					hostAlpha,
				},
			}, nil,
		)
		assert.Empty(t, response.Failures)
		require.NotNil(t, response.LocalStats)
		assert.False(t, response.LocalStats.Aborted)
		assert.Equal(t, 3, response.LocalStats.Synced)
		select {
		case event := <-events:
			assert.Equal(t, "sync", event.Scope)
		case <-time.After(time.Second):
			require.FailNow(t, "unified rebuild did not emit a sync event")
		}
	}

	assert.Equal(t, 2, archiveRequests,
		"unchanged full rebuild should reuse the prepared mirror")
	assertSessionCount(t, f.db, 3)
	output := logs.String()
	assert.Contains(t, output,
		"remote sync HTTP contributors started: hosts=2")
	assert.Contains(t, output,
		"remote sync HTTP contributors finished: hosts=2")
	for _, host := range []string{"alpha", "gamma"} {
		assert.Equal(t, 2, strings.Count(output,
			"remote sync HTTP host preparation started: host="+host))
		assert.Equal(t, 2, strings.Count(output,
			"remote sync HTTP host preparation finished: host="+host))
		assert.Regexp(t, "remote sync HTTP host preparation finished: host="+host+
			`[^\n]*duration=[^\n]*outcome=completed`,
			output,
		)
		assert.Equal(t, 2, strings.Count(output,
			"remote sync host started: host="+host+
				" transport=http full=true mode=unified_rebuild"))
		assert.Equal(t, 2, strings.Count(output,
			"remote sync host finished: host="+host+" transport=http"))
		assert.Regexp(t, "remote sync host finished: host="+host+
			` transport=http[^\n]*sessions_synced=1`+
			`[^\n]*sessions_total=1[^\n]*outcome=completed`,
			output,
		)
	}
	assert.Contains(t, output, "aggregate_synced=3")
	assert.NotContains(t, output, "local_synced=")
	select {
	case err := <-serverErrors:
		require.NoError(t, err)
	default:
	}
}

func TestRunRemoteSyncRequestHTTPPreparationFailurePreservesArchive(t *testing.T) {
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

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
			},
		}, nil,
	)

	require.Len(t, response.Failures, 1)
	assert.Equal(t, "alpha", response.Failures[0].Host.Host)
	assert.Contains(t, response.Failures[0].Err, "403 Forbidden")
	assert.NotContains(t, response.Failures[0].Err, remoteCause.Detail)
	assert.Equal(t, 1, prepared.closed)
	assertSessionCount(t, f.db, 0)
}

func TestRunRemoteSyncRequestAbortedUnifiedRebuildReturnsTopLevelError(t *testing.T) {
	f := newSyncRouteFixture(t)
	missingPath := filepath.Join(f.dir, "missing.jsonl")
	require.NoError(t, f.db.UpsertSession(t.Context(), db.Session{
		ID: "preserved-old-session", Agent: "claude", Machine: "local",
		Project: "preserved", FilePath: &missingPath, MessageCount: 1,
	}))
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		return &fakePreparedHTTPRebuild{}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{
			Full: true, IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
			},
		}, nil,
	)

	require.NotNil(t, response.LocalStats)
	assert.True(t, response.LocalStats.Aborted)
	assert.Equal(t, "unified local and HTTP rebuild aborted", response.Error)
	assert.Empty(t, response.Failures,
		"an aggregate rebuild abort is not a host failure")
	preserved, err := f.db.GetSession(t.Context(), "preserved-old-session")
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

	assert.Equal(t, "HTTP remote sync blocked: cleanup from an earlier sync still owns resources",
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
	logs := captureServerLogOutput(t)
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		return &fakePreparedHTTPRebuild{}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	response := f.srv.runRemoteSyncRequest(
		ctx, f.db, f.srv.syncEngineForLocal(ctx, f.db),
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
	output := logs.String()
	assert.Contains(t, output,
		"remote sync request started: include_local=true full=true hosts=1")
	assert.Contains(t, output, "remote sync request finished: include_local=true full=true")
	assert.Contains(t, output, "duration=")
	assert.Contains(t, output, "outcome=canceled")
	assert.Contains(t, output, "error=\"context canceled\"")
	assert.NotContains(t, output, "secret")
}

func TestRunRemoteSyncRequestLogsPerHostLifecycle(t *testing.T) {
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

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{
			Full: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret-token"},
				{Host: "beta", Transport: config.RemoteTransportHTTP, Token: "secret-token"},
			},
		}, nil,
	)

	require.Empty(t, response.Error)
	require.Len(t, response.Failures, 1)
	assert.Equal(t, "beta", response.Failures[0].Host.Host)
	output := logs.String()
	assert.Contains(t, output,
		"remote sync host started: host=alpha transport=http full=true")
	assert.Contains(t, output,
		"remote sync host finished: host=alpha transport=http")
	assert.Contains(t, output, "sessions_synced=3")
	assert.Contains(t, output, "outcome=completed")
	assert.Contains(t, output,
		"remote sync host started: host=beta transport=http full=true")
	assert.Contains(t, output,
		"remote sync host finished: host=beta transport=http")
	assert.Contains(t, output, "outcome=failed")
	assert.Contains(t, output, "error=\"HTTP remote sync failed\"")
	assert.NotContains(t, output, privateURL)
	assert.NotContains(t, output, "secret-token")
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
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
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

func TestRunRemoteSyncRequestCombinesRemoteAggregateStats(t *testing.T) {
	f := newSyncRouteFixture(t)
	logs := captureServerLogOutput(t)
	stubRunHTTPRemoteSync(t, func(
		_ context.Context, rh config.RemoteHost, _ bool,
	) (remotesync.SyncStats, error) {
		if rh.Host == "alpha" {
			return remotesync.SyncStats{
				SessionsSynced: 2, SessionsTotal: 3, Skipped: 1,
			}, nil
		}
		assert.Equal(t, "beta", rh.Host)
		return remotesync.SyncStats{
			SessionsSynced: 4, SessionsTotal: 5, Skipped: 2, Failed: 1,
		}, nil
	})

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP},
				{Host: "beta"},
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
				t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
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
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{Full: true, Hosts: []config.RemoteHost{{
			Host: "alpha", Transport: config.RemoteTransportHTTP,
		}}}, nil,
	)

	assert.Empty(t, response.Failures)
	assert.Zero(t, prepareCalls)
	assert.Equal(t, 1, activeCalls)
}

func TestPrepareHTTPRebuildOmitsOfflineHost(t *testing.T) {
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

	require.NoError(t, err)
	require.NotNil(t, prepared)
	options, release, err := prepared.BorrowRebuildOptions(t.Context())
	require.NoError(t, err)
	assert.Empty(t, options.Contributors)
	assert.Equal(t, []string{"offline~"},
		options.UnavailableContributorIDPrefixes)
	release()
	require.NoError(t, prepared.Close())
	assert.Contains(t, progress, syncpkg.Progress{
		Detail: "Skipped offline remote host offline",
	})
}

func TestRunRemoteSyncRequestRebuildContinuesAfterDNSFailure(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "explicit"
		if automatic {
			name = "data-version-upgrade"
		}
		t.Run(name, func(t *testing.T) {
			var opts []syncRouteFixtureOption
			if automatic {
				opts = append(opts, withStaleDB())
			}
			f := newSyncRouteFixture(t, opts...)
			f.writeClaudeSession(t, "proj/local.jsonl", "fresh local session")
			missingPath := filepath.Join(f.dir, "offline.jsonl")
			require.NoError(t, f.db.UpsertSession(t.Context(), db.Session{
				ID: "offline~session", Agent: "claude", Machine: "offline",
				Project: "archive", FilePath: &missingPath, MessageCount: 1,
			}))

			prepare := prepareHTTPRebuild
			stubPrepareHTTPRebuild(t, func(
				ctx context.Context, syncs []remotesync.HTTPSync,
			) (preparedHTTPRebuild, error) {
				require.Len(t, syncs, 1)
				for i := range syncs {
					syncs[i].Client = &http.Client{Transport: offlineRemoteTransport{
						err: &net.DNSError{Err: "no such host", Name: "offline.invalid", IsNotFound: true},
					}}
				}
				return prepare(ctx, syncs)
			})
			var progress []syncpkg.Progress
			response := f.srv.runRemoteSyncRequest(
				t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
				remoteSyncRequest{
					Full: !automatic, IncludeLocal: true,
					Hosts: []config.RemoteHost{{
						Host: "offline", URL: "http://offline.invalid", Token: "token",
					}},
				}, func(p syncpkg.Progress) { progress = append(progress, p) },
			)

			require.Empty(t, response.Error)
			require.Empty(t, response.Failures)
			require.NotNil(t, response.LocalStats)
			assert.False(t, response.LocalStats.Aborted)
			assert.False(t, f.db.NeedsResync())
			local, err := f.db.GetSession(t.Context(), "local")
			require.NoError(t, err)
			require.NotNil(t, local, "local imports must proceed when the remote cannot resolve")
			assert.Equal(t, "fresh local session", *local.FirstMessage)
			preserved, err := f.db.GetSession(t.Context(), "offline~session")
			require.NoError(t, err)
			assert.NotNil(t, preserved, "offline remote history must survive the rebuild")
			assert.Contains(t, progress, syncpkg.Progress{Detail: "Skipped offline remote host offline"})
		})
	}
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
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{IncludeLocal: true, Hosts: []config.RemoteHost{{
			Host: "alpha", Transport: config.RemoteTransportHTTP,
		}}}, nil,
	)

	assert.Empty(t, response.Failures)
	assert.Zero(t, prepareCalls)
	assert.Equal(t, 1, activeCalls)
}

func TestRunRemoteSyncRequestConfiguredSkipsOfflineHTTPHost(t *testing.T) {
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
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
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
	assert.Empty(t, response.Error)
	assert.Empty(t, response.Failures)
	assert.Equal(t, []string{"offline", "reachable"}, calls)
	assert.Contains(t, progress, syncpkg.Progress{
		Detail: "Skipped offline remote host offline",
	})
}

func TestRunRemoteSyncRequestExplicitHostKeepsOfflineFailure(t *testing.T) {
	f := newSyncRouteFixture(t)
	stubRunHTTPRemoteSync(t, func(
		context.Context, config.RemoteHost, bool,
	) (remotesync.SyncStats, error) {
		return remotesync.SyncStats{}, syscall.ETIMEDOUT
	})

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{Hosts: []config.RemoteHost{{
			Host: "offline", Transport: config.RemoteTransportHTTP,
		}}}, nil,
	)

	assert.Empty(t, response.Error)
	require.Len(t, response.Failures, 1)
	assert.Equal(t, "offline", response.Failures[0].Host.Host)
	assert.Contains(t, response.Failures[0].Err, "connection timed out")
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
				t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
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
			errors.New("cleanup still blocks next request"),
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
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{
			IncludeLocal: true, Hosts: []config.RemoteHost{httpHost("alpha"), httpHost("beta")},
		}, nil,
	)
	require.Len(t, first.Failures, 1)
	assert.Equal(t, []string{"alpha"}, callbacks)
	assert.Equal(t, 1, owner.retries)

	second := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{
			IncludeLocal: true, Hosts: []config.RemoteHost{httpHost("gamma")},
		}, nil,
	)
	assert.Empty(t, second.Failures)
	assert.Equal(t, "HTTP remote sync blocked: cleanup from an earlier sync still owns resources", second.Error)
	assert.Equal(t, []string{"alpha"}, callbacks)
	assert.Equal(t, 2, owner.retries)

	third := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{
			IncludeLocal: true, Hosts: []config.RemoteHost{httpHost("delta")},
		}, nil,
	)
	assert.Empty(t, third.Error)
	assert.Empty(t, third.Failures)
	assert.Equal(t, []string{"alpha", "delta"}, callbacks)
	assert.Equal(t, 3, owner.retries)
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

	response := f.srv.runRemoteSyncRequest(
		t.Context(), f.db, f.srv.syncEngineForLocal(t.Context(), f.db),
		remoteSyncRequest{
			IncludeLocal: true,
			Hosts: []config.RemoteHost{
				{Host: "alpha", Transport: config.RemoteTransportHTTP, Token: "secret"},
			},
		}, nil,
	)

	assert.Empty(t, response.Failures)
	assert.Equal(t, 1, prepareCalls)
	assert.Zero(t, activeHTTPCalls)
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
			engines[i] = srv.syncEngineForLocal(t.Context(), database)
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
		run  func(*Server, context.Context, func() error) error
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
				done <- tc.run(f.srv, t.Context(), func() error {
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
				payload, err := json.Marshal(request.body)
				require.NoError(t, err)
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, request.path, bytes.NewReader(payload))
				req.Host = "127.0.0.1:0"
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Origin", "http://127.0.0.1:0")
				req.Header.Set("Content-Type", "application/json")
				response := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					w := httptest.NewRecorder()
					f.handler.ServeHTTP(w, req)
					response <- w
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
	f := newSyncRouteFixture(t, withUsageOnlyStorage())
	f.writeClaudeSession(t, "proj/private.jsonl", "private prompt")

	stats := f.srv.syncEngineForLocal(t.Context(), f.db).SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced)

	page, err := f.db.ListSessions(t.Context(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Nil(t, page.Sessions[0].FirstMessage)

	messages, err := f.db.GetAllMessages(t.Context(), page.Sessions[0].ID)
	require.NoError(t, err)
	assert.Empty(t, messages,
		"a session without usage or assistant activity needs no message rows")
	for _, message := range messages {
		assert.Empty(t, message.Content)
		assert.Empty(t, message.ThinkingText)
		assert.Empty(t, message.ToolCalls)
		assert.Empty(t, message.ToolResults)
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
	engine := srv.syncEngineForLocal(t.Context(), database)
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
	ran := false
	f := newSyncRouteFixture(t, withStaleDB(), withLocalSyncRunner(
		func(context.Context, func(syncpkg.Progress)) (syncpkg.SyncStats, error) {
			ran = true
			return syncpkg.SyncStats{}, nil
		},
	))
	require.True(t, f.db.NeedsResync())

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync", nil)

	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "resync")
	assert.Equal(t, "true", w.Header().Get(ResyncRequiredHeader),
		"the rejection must carry the machine-readable resync signal for the CLI")
	assert.False(t, ran, "the worker-backed runner must not run for a stale archive")
	assert.True(t, f.db.NeedsResync(), "a rejected sync must not resync")
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
			engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
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
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
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
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	stubRunHTTPRemoteSync(t, func(
		context.Context,
		config.RemoteHost,
		bool,
	) (remotesync.SyncStats, error) {
		return remotesync.SyncStats{SessionsSynced: 1}, nil
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
	f := newSyncRouteFixture(t, withRemoteHosts(config.RemoteHost{
		Host: "alpha", URL: "https://alpha.example.test", Token: "remote-token",
	}))
	f.writeClaudeSession(t, "project/remote-progress.jsonl", "remote progress")
	stubPrepareHTTPRebuild(t, func(
		context.Context, []remotesync.HTTPSync,
	) (preparedHTTPRebuild, error) {
		return &fakePreparedHTTPRebuild{}, nil
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
	assertOnlySessionFirstMessageContains(t, f.db, "remote progress")
}

func TestHumaSyncRemotesStreamsRemoteProgress(t *testing.T) {
	var mu stdlibsync.Mutex
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer remote-token", r.Header.Get("Authorization"))
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		remotesync.SetProtocolHeader(w.Header())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			_, _ = w.Write([]byte(`{}`))
		case "/api/v1/remote-sync/manifest":
			_, _ = w.Write([]byte(`{"files":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	f := newSyncRouteFixture(t, withRemoteHosts(config.RemoteHost{
		Host: "alpha", URL: ts.URL, Token: "remote-token",
	}))

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/sync/remotes",
		remoteSyncRequest{
			Hosts: []config.RemoteHost{{
				Host: "alpha", URL: "http://unconfigured.example", Token: "wrong-token",
			}},
		},
		withAccept("text/event-stream"),
	)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/event-stream")
	body := w.Body.String()
	assert.Contains(t, body, "event: progress")
	assert.Contains(t, body, "Resolving agent directories on alpha")
	assert.Contains(t, body, "event: done")
	assert.NotContains(t, body, `"failures"`)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{
		"/api/v1/remote-sync/targets",
		"/api/v1/remote-sync/manifest",
	}, paths)
}

func TestRunRemoteSyncRequestSerializesNoSyncRemoteWrites(t *testing.T) {
	f := newSyncRouteFixture(t)
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)

	remoteEntered := make(chan struct{})
	releaseRemote := make(chan struct{})
	var remoteOnce stdlibsync.Once
	stubRunHTTPRemoteSync(t, func(
		context.Context,
		config.RemoteHost,
		bool,
	) (remotesync.SyncStats, error) {
		remoteOnce.Do(func() { close(remoteEntered) })
		<-releaseRemote
		return remotesync.SyncStats{}, nil
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

func TestHumaSyncRemotesRejectsUnconfiguredHost(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:1234", "203.0.113.10:1234"} {
		t.Run(addr, func(t *testing.T) {
			f := newSyncRouteFixture(t)
			w := postRemoteSync(t, f.handler,
				[]config.RemoteHost{{Host: "unconfigured-box"}},
				withRemoteAddr(addr))

			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Contains(t, w.Body.String(), "not configured in remote_hosts")
		})
	}
}

func TestHumaSyncRemotesAllowsNonLocalConfiguredHostByName(t *testing.T) {
	allowed := config.RemoteHost{
		Host:     "allowed-box",
		URL:      "https://allowed.example.test",
		Token:    "configured-token",
		Interval: 5 * time.Minute,
	}
	requested := config.RemoteHost{
		Host: "allowed-box",
	}
	f := newSyncRouteFixture(t, withRemoteHosts(allowed))

	var got config.RemoteHost
	stubRunHTTPRemoteSync(t, func(
		_ context.Context,
		rh config.RemoteHost,
		_ bool,
	) (remotesync.SyncStats, error) {
		got = rh
		return remotesync.SyncStats{SessionsSynced: 1, SessionsTotal: 1}, nil
	})
	w := postRemoteSync(t, f.handler, []config.RemoteHost{requested},
		withRemoteAddr("192.168.1.50:1234"))

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, allowed, got)
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
	remoteRoot := t.TempDir()
	remoteSession := filepath.Join(
		remoteRoot, "tmp", "project", "chats", "session-remote.json",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(remoteSession), 0o755))
	require.NoError(t, os.WriteFile(remoteSession, []byte(testjsonl.GeminiSessionJSON(
		"remote-gemini", "project",
		"2026-08-09T10:00:00Z", "2026-08-09T10:01:00Z",
		[]map[string]any{testjsonl.GeminiUserMsg(
			"user", "2026-08-09T10:00:00Z", "import remote session",
		)},
	)), 0o644))
	targets := remotesync.TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentGemini: {remoteRoot},
	}}
	manifest, err := remotesync.BuildManifest(t.Context(), targets)
	require.NoError(t, err)
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

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	close(serverErrors)
	for serverErr := range serverErrors {
		require.NoError(t, serverErr)
	}
	stored, err := database.GetSession(
		t.Context(), "devbox~gemini:remote-gemini",
	)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, string(parser.AgentGemini), stored.Agent)
}

func TestOnDemandSyncEngineExcludesDisabledProvider(t *testing.T) {
	geminiDir := filepath.Join(t.TempDir(), "gemini")
	f := newSyncRouteFixture(t, withDisabledAgents(
		[]parser.AgentType{parser.AgentGemini},
		map[parser.AgentType][]string{parser.AgentGemini: {geminiDir}},
	))

	engine := f.srv.syncEngineForLocal(t.Context(), f.db)

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

	engine := f.srv.syncEngineForLocal(t.Context(), f.db)

	assert.Equal(t, []string{geminiDir},
		engine.ReconciliationRootsForAgent(string(parser.AgentGemini)))
}
