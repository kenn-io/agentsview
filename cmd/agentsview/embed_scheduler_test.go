package main

import (
	"context"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/vector"
)

// --- fake embedManager ---

// fakeEmbedManager records every TryBuild call and returns scripted
// (started, err) results in order, repeating the last scripted result once
// the script is exhausted (default: started=true, err=nil).
type fakeEmbedManager struct {
	mu      sync.Mutex
	calls   []vector.BuildRequest
	results []fakeTryBuildResult
}

type fakeTryBuildResult struct {
	started bool
	err     error
}

func (f *fakeEmbedManager) TryBuild(
	_ context.Context, req vector.BuildRequest,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	idx := len(f.calls) - 1
	if idx < len(f.results) {
		r := f.results[idx]
		return r.started, r.err
	}
	if len(f.results) > 0 {
		r := f.results[len(f.results)-1]
		return r.started, r.err
	}
	return true, nil
}

func (f *fakeEmbedManager) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeEmbedManager) callsSnapshot() []vector.BuildRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]vector.BuildRequest(nil), f.calls...)
}

// waitForSchedulerCondition polls cond until it is true or 2s pass, failing
// the test with msg otherwise. Avoids fixed sleeps that would either flake
// under load or slow the suite down needlessly.
func waitForSchedulerCondition(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.Fail(t, "timed out waiting for condition", msg)
}

func TestEmbedSchedulerBurstOfNotifyProducesExactlyOneBuild(t *testing.T) {
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, 20*time.Millisecond, 0)

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	for range 10 {
		s.Notify()
		time.Sleep(2 * time.Millisecond)
	}

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"expected a build after the burst quieted")
	// Give any spurious extra build a chance to show up before asserting
	// there is exactly one.
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, 1, fake.callCount(), "a burst of Notify must collapse to one build")
	assert.Equal(t, []vector.BuildRequest{{}}, fake.callsSnapshot())
}

func TestEmbedSchedulerNotifyDuringRunningBuildRearmsForFollowUpPass(t *testing.T) {
	fake := &fakeEmbedManager{
		results: []fakeTryBuildResult{
			{started: false, err: nil}, // a build is already running elsewhere
			{started: true, err: nil},  // the follow-up pass actually runs
		},
	}
	s := newEmbedScheduler(fake, 15*time.Millisecond, 0)

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	s.Notify()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 2 },
		"expected the scheduler to re-arm and retry after a dropped build")
	calls := fake.callsSnapshot()
	require.Len(t, calls, 2)
	assert.Equal(t, vector.BuildRequest{}, calls[0])
	assert.Equal(t, vector.BuildRequest{}, calls[1])
}

func TestEmbedSchedulerBackstopTickIssuesBackstopBuild(t *testing.T) {
	fake := &fakeEmbedManager{}
	// A very long debounce so only the backstop ticker can fire a build.
	s := newEmbedScheduler(fake, time.Hour, 20*time.Millisecond)

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"expected a backstop build")
	calls := fake.callsSnapshot()
	require.NotEmpty(t, calls)
	assert.True(t, calls[0].Backstop, "backstop tick must set BuildRequest.Backstop")
}

// TestEmbedSchedulerDroppedBackstopRetriesOnNextDebouncedBuild is the fix-4
// regression test: a backstop tick that collides with a build already
// running elsewhere must not be silently dropped for a full backstop
// interval (24h in production). The scheduler must remember it and fold
// Backstop: true into the next debounced build request instead.
func TestEmbedSchedulerDroppedBackstopRetriesOnNextDebouncedBuild(t *testing.T) {
	fake := &fakeEmbedManager{
		results: []fakeTryBuildResult{
			{started: false, err: nil}, // the backstop tick collides with a build elsewhere
			{started: true, err: nil},  // the following debounced build recovers it
		},
	}
	// A long backstop interval relative to the debounce interval and the
	// test's own buffers keeps a second, unrelated backstop tick from
	// firing mid-test and making the call count non-deterministic.
	s := newEmbedScheduler(fake, 10*time.Millisecond, 500*time.Millisecond)

	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"expected the backstop tick to fire and be dropped")

	s.Notify()

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 2 },
		"expected the debounced build to recover the dropped backstop")
	time.Sleep(50 * time.Millisecond)

	calls := fake.callsSnapshot()
	require.Len(t, calls, 2, "the dropped backstop must be retried exactly once, not repeatedly")
	assert.True(t, calls[0].Backstop, "the original (dropped) backstop tick request")
	assert.True(t, calls[1].Backstop,
		"the debounced build must carry the pending backstop forward instead of dropping it")

	// Once the recovered build actually started, the pending flag must
	// clear: a further, unrelated debounced build must not keep carrying
	// Backstop: true forever.
	s.Notify()
	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 3 },
		"expected a further debounced build after the recovered one")
	time.Sleep(50 * time.Millisecond)

	calls = fake.callsSnapshot()
	require.Len(t, calls, 3)
	assert.False(t, calls[2].Backstop,
		"the recovered backstop must not leak into a later unrelated debounced build")
}

func TestEmbedSchedulerStopTerminatesRun(t *testing.T) {
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, time.Hour, 0)

	go s.Run(context.Background())

	// Stop blocks until Run has actually exited, so its returning at all
	// (within a generous timeout) is the proof Run terminated.
	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		require.Fail(t, "Stop did not return; Run likely never terminated")
	}
}

func TestEmbedSchedulerNotifyNeverBlocksWithoutAReader(t *testing.T) {
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, time.Hour, 0)

	done := make(chan struct{})
	go func() {
		for range 100 {
			s.Notify()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		require.Fail(t, "Notify blocked with no Run consuming dirty")
	}
}

// --- teeEmitter ---

type recordingEmitter struct {
	mu     sync.Mutex
	scopes []string
}

func (e *recordingEmitter) Emit(scope string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.scopes = append(e.scopes, scope)
}

func (e *recordingEmitter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.scopes)
}

func TestTeeEmitterAlwaysCallsPrimaryAndGatesSchedulerOnRunAfterSync(t *testing.T) {
	primary := &recordingEmitter{}
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, 10*time.Millisecond, 0)
	ctx := t.Context()
	go s.Run(ctx)
	defer s.Stop()

	disabled := teeEmitter{primary: primary, scheduler: s, runAfterSync: false}
	disabled.Emit("sessions")
	assert.Equal(t, 1, primary.count())
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, 0, fake.callCount(), "runAfterSync=false must not notify the scheduler")

	enabled := teeEmitter{primary: primary, scheduler: s, runAfterSync: true}
	enabled.Emit("sessions")
	assert.Equal(t, 2, primary.count())
	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"runAfterSync=true must notify the scheduler")
}

// TestRunRemoteHostSyncLoop_EmitsThroughTeeNotifiesScheduler is a
// regression test for a bug where runServe wired startPeriodicSync's
// scheduled remote-host sync path to the bare SSE broadcaster instead of
// the wrapped teeEmitter, so remote-synced sessions notified SSE clients
// but never reached the embed scheduler until an unrelated local sync or
// the 24h backstop ran. It drives runRemoteHostSyncLoop — the function
// startPeriodicSync spawns per configured remote host — with a teeEmitter
// and a syncFn reporting synced sessions, and asserts the scheduler
// actually receives a build trigger through that path.
func TestRunRemoteHostSyncLoop_EmitsThroughTeeNotifiesScheduler(t *testing.T) {
	primary := &recordingEmitter{}
	fake := &fakeEmbedManager{}
	s := newEmbedScheduler(fake, 5*time.Millisecond, 0)
	schedCtx := t.Context()
	go s.Run(schedCtx)
	defer s.Stop()

	tee := teeEmitter{primary: primary, scheduler: s, runAfterSync: true}

	syncFn := func() (int, error) {
		return 1, nil // one session synced on this remote host
	}

	loopCtx := t.Context()
	done := make(chan struct{})
	go runRemoteHostSyncLoop(
		loopCtx, "remote-host", 5*time.Millisecond, syncFn, tee, nil, done,
	)
	defer close(done)

	waitForSchedulerCondition(t, func() bool { return fake.callCount() >= 1 },
		"a scheduled remote sync reaching the tee emitter must notify the embed scheduler")
	assert.NotZero(t, primary.count(),
		"the tee must still forward remote sync completions to the SSE broadcaster")
}

// --- searcherAdapter error taxonomy ---

func TestTranslateSearchErrorMapsVectorErrorsToSemanticUnavailable(t *testing.T) {
	t.Run("no active generation", func(t *testing.T) {
		err := translateSearchError(vector.ErrNoActiveGeneration)
		assert.ErrorIs(t, err, db.ErrSemanticUnavailable)
	})
	t.Run("building", func(t *testing.T) {
		err := translateSearchError(&vector.BuildingError{Percent: 62})
		assert.ErrorIs(t, err, db.ErrSemanticUnavailable)
		assert.Contains(t, err.Error(), "index is building: 62% complete")
	})
	t.Run("other error passes through", func(t *testing.T) {
		boom := errors.New("boom")
		assert.Same(t, boom, translateSearchError(boom))
	})
	t.Run("query encode failure maps to semantic transient, not unavailable", func(t *testing.T) {
		queryErr := &vector.QueryEncodeError{Err: errors.New("dial tcp: connection refused")}
		got := translateSearchError(queryErr)
		assert.ErrorIs(t, got, db.ErrSemanticTransient)
		assert.False(t, errors.Is(got, db.ErrSemanticUnavailable),
			"a query-time endpoint failure must not read as semantic search being disabled")
		assert.Contains(t, got.Error(), "connection refused")
	})
}

// --- integration: real serve/server construction path ---

// vectorTestConfig returns a config.Config with a small-but-valid [vector]
// section (accepted by config.Validate) pointed at an embeddings endpoint
// that is never actually called in these tests — setupVectorServing only
// constructs the encoder, it does not exercise it.
func vectorTestConfig(dataDir string) config.Config {
	return config.Config{
		DataDir: dataDir,
		DBPath:  filepath.Join(dataDir, "sessions.db"),
		Vector: config.VectorConfig{
			Enabled: true,
			Embeddings: config.VectorEmbeddingsConfig{
				Endpoint:      "http://127.0.0.1:1/v1",
				Model:         "test-model",
				Dimension:     3,
				BatchSize:     10,
				Timeout:       "5s",
				MaxRetries:    1,
				MaxInputChars: 1000,
			},
			Embed: config.VectorEmbedConfig{
				BackstopInterval: "24h",
			},
		},
	}
}

// listenLoopback opens a loopback listener on an OS-assigned port,
// returning it alongside the port so a caller can set cfg.Port to it
// before constructing the server: the host-check middleware validates
// the Host header against cfg.Host:cfg.Port exactly, so the request's
// destination port must be known up front rather than assigned by
// httptest.NewServer after the fact.
func listenLoopback(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return ln, ln.Addr().(*net.TCPAddr).Port
}

// startTestServer serves handler on ln (already bound to the port cfg.Port
// was set to) and returns an *httptest.Server whose URL and lifecycle
// helpers work as usual.
func startTestServer(t *testing.T, ln net.Listener, handler http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(handler)
	require.NoError(t, ts.Listener.Close())
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// TestServeConstructionRegistersEmbeddingsRoutesWhenVectorEnabled drives the
// real setupVectorServing + server.New construction path (not a fake mux)
// with a vector-enabled config and asserts the embeddings status endpoint
// responds, then a sibling test rebuilds with vector disabled and asserts
// it does not.
func TestServeConstructionRegistersEmbeddingsRoutesWhenVectorEnabled(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	ln, port := listenLoopback(t)
	cfg := vectorTestConfig(dataDir)
	cfg.Host, cfg.Port = "127.0.0.1", port
	vs, err := setupVectorServing(context.Background(), cfg, database)
	require.NoError(t, err)
	require.NotNil(t, vs.Scheduler)
	require.NotNil(t, vs.Close)
	defer func() { require.NoError(t, vs.Close()) }()

	srv := server.New(cfg, database, nil, vs.ServerOpts...)
	ts := startTestServer(t, ln, srv.Handler())

	resp, err := http.Get(ts.URL + "/api/v1/embeddings/status")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", mediaType(t, resp.Header.Get("Content-Type")))
}

// TestEmbeddingsDaemonClientBuildSucceedsThroughRealMiddleware drives a POST
// build request from embeddingsDaemonClient (the same client `embeddings
// build` uses to talk to a running daemon) through the server's real
// middleware chain (srv.Handler(), not a bare mux), proving the CSRF guard
// in internal/server/server.go's corsMiddleware does not reject it. Every
// other embeddings-daemon-dispatch test in embeddings_test.go serves off an
// httptest.NewServeMux() directly, bypassing that middleware entirely, which
// is exactly how the missing Origin header regression went unnoticed.
func TestEmbeddingsDaemonClientBuildSucceedsThroughRealMiddleware(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	ln, port := listenLoopback(t)
	cfg := vectorTestConfig(dataDir)
	cfg.Host, cfg.Port = "127.0.0.1", port
	vs, err := setupVectorServing(context.Background(), cfg, database)
	require.NoError(t, err)
	defer func() { require.NoError(t, vs.Close()) }()

	srv := server.New(cfg, database, nil, vs.ServerOpts...)
	ts := startTestServer(t, ln, srv.Handler())

	client := embeddingsDaemonClient{baseURL: ts.URL}
	err = client.startBuild(context.Background(), vector.BuildRequest{})
	require.NoError(t, err,
		"a POST build must succeed once the client sets Origin to satisfy the CSRF guard")
}

// TestSetupVectorServingDisablesWhenWriteLockHeld asserts a held
// vectors.write.lock (simulating a concurrent direct `embeddings build`)
// makes setupVectorServing degrade to a fully-disabled vectorServing after a
// short retry window, rather than blocking or failing daemon startup, and
// that it logs a clear warning explaining why.
func TestSetupVectorServingDisablesWhenWriteLockHeld(t *testing.T) {
	origInterval, origTimeout := vectorsWriteLockRetryInterval, vectorsWriteLockRetryTimeout
	vectorsWriteLockRetryInterval = time.Millisecond
	vectorsWriteLockRetryTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		vectorsWriteLockRetryInterval = origInterval
		vectorsWriteLockRetryTimeout = origTimeout
	})

	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	cfg := vectorTestConfig(dataDir)

	held, err := tryAcquireNamedLock(dataDir, vectorsWriteLockFile)
	require.NoError(t, err)
	defer func() { require.NoError(t, held.Close()) }()

	logBuf := captureLogOutput(t)

	vs, err := setupVectorServing(context.Background(), cfg, database)
	require.NoError(t, err, "a held lock must degrade, not fail, daemon startup")
	assert.Nil(t, vs.Scheduler)
	assert.Nil(t, vs.Close)
	assert.Empty(t, vs.ServerOpts)
	assert.Contains(t, logBuf.String(), "vectors.write.lock")
	assert.Contains(t, logBuf.String(), "disabling vector serving")
}

// TestSetupVectorServingAcquiresAndReleasesWriteLock asserts a free
// vectors.write.lock is acquired for the daemon's lifetime and released by
// Close, so a second setupVectorServing call after Close succeeds rather
// than finding the lock still held.
func TestSetupVectorServingAcquiresAndReleasesWriteLock(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	cfg := vectorTestConfig(dataDir)

	vs, err := setupVectorServing(context.Background(), cfg, database)
	require.NoError(t, err)
	require.NotNil(t, vs.Close)

	// While vs holds the lock, a competing direct acquire must fail.
	_, lockErr := tryAcquireNamedLock(dataDir, vectorsWriteLockFile)
	require.Error(t, lockErr, "setupVectorServing must hold vectors.write.lock while running")

	require.NoError(t, vs.Close())

	// Once released, a fresh acquire — standing in for a second
	// setupVectorServing call — must succeed.
	held, err := tryAcquireNamedLock(dataDir, vectorsWriteLockFile)
	require.NoError(t, err, "Close must release vectors.write.lock")
	require.NoError(t, held.Close())
}

func TestServeConstructionOmitsEmbeddingsRoutesWhenVectorDisabled(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	ln, port := listenLoopback(t)
	cfg := config.Config{DataDir: dataDir, DBPath: filepath.Join(dataDir, "sessions.db"),
		Host: "127.0.0.1", Port: port}
	vs, err := setupVectorServing(context.Background(), cfg, database)
	require.NoError(t, err)
	assert.Nil(t, vs.Scheduler)
	assert.Nil(t, vs.Close)
	assert.Empty(t, vs.ServerOpts)

	srv := server.New(cfg, database, nil, vs.ServerOpts...)
	ts := startTestServer(t, ln, srv.Handler())

	resp, err := http.Get(ts.URL + "/api/v1/embeddings/status")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.NotEqual(t, "application/json", mediaType(t, resp.Header.Get("Content-Type")),
		"the embeddings API must not be registered when vector is disabled")
}

// mediaType extracts the bare MIME type from a Content-Type header value,
// dropping any "; charset=..." parameters, so tests can compare it exactly.
func mediaType(t *testing.T, contentType string) string {
	t.Helper()
	mt, _, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)
	return mt
}

// --- installDirectVectorSearcher (direct, non-daemon CLI path) ---

func TestInstallDirectVectorSearcherNoOpWhenVectorDisabled(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	cfg := config.Config{DataDir: dataDir}
	closeFn := installDirectVectorSearcher(cfg, database)
	assert.Nil(t, closeFn)
	assert.False(t, database.HasSemantic())
}

func TestInstallDirectVectorSearcherNoOpWhenVectorsDBMissing(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	cfg := vectorTestConfig(dataDir)
	closeFn := installDirectVectorSearcher(cfg, database)
	assert.Nil(t, closeFn)
	assert.False(t, database.HasSemantic(),
		"no searcher wired means callers see db.ErrSemanticUnavailable naturally")
}

func TestInstallDirectVectorSearcherWiresSearcherWhenVectorsDBExists(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	cfg := vectorTestConfig(dataDir)

	// Create vectors.db up front, as a prior `embeddings build` would.
	seed, err := vector.Open(context.Background(), cfg.Vector.ResolvedDBPath(dataDir), false, 1000)
	require.NoError(t, err)
	require.NoError(t, seed.Close())

	closeFn := installDirectVectorSearcher(cfg, database)
	require.NotNil(t, closeFn)
	defer func() { assert.NoError(t, closeFn()) }()

	assert.True(t, database.HasSemantic(),
		"an existing vectors.db must wire a read-only searcher for direct CLI reads")
}

// TestInstallDirectVectorSearcherDegradesOnCorruptVectorsDB is a regression
// test for a bug where a corrupt or incompatible vectors.db broke direct
// service construction entirely, taking down unrelated commands like
// `session list` that never touch the vector index. A garbage vectors.db
// file must degrade to "no searcher installed" rather than propagate an
// error, leaving non-semantic reads unaffected and semantic search falling
// back to the standard unavailable error.
func TestInstallDirectVectorSearcherDegradesOnCorruptVectorsDB(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	cfg := vectorTestConfig(dataDir)

	// Not a SQLite file at all — simulates corruption or an incompatible
	// build rather than a partially-written one.
	vectorsPath := cfg.Vector.ResolvedDBPath(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(vectorsPath), 0o755))
	require.NoError(t, os.WriteFile(vectorsPath, []byte("not a sqlite database"), 0o644))

	closeFn := installDirectVectorSearcher(cfg, database)
	assert.Nil(t, closeFn,
		"a corrupt vectors.db must not return a handle to close")
	assert.False(t, database.HasSemantic(),
		"a corrupt vectors.db must degrade to no searcher, not fail construction")

	_, err := database.SearchContent(context.Background(), db.ContentSearchFilter{
		Pattern: "query",
		Mode:    "semantic",
		Limit:   5,
	})
	assert.ErrorIs(t, err, db.ErrSemanticUnavailable,
		"semantic search must return the standard unavailable error once degraded")
}
