package main

import (
	"context"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
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
	closeFn, err := installDirectVectorSearcher(cfg, database)
	require.NoError(t, err)
	assert.Nil(t, closeFn)
	assert.False(t, database.HasSemantic())
}

func TestInstallDirectVectorSearcherNoOpWhenVectorsDBMissing(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))

	cfg := vectorTestConfig(dataDir)
	closeFn, err := installDirectVectorSearcher(cfg, database)
	require.NoError(t, err, "a missing vectors.db must not be an error")
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

	closeFn, err := installDirectVectorSearcher(cfg, database)
	require.NoError(t, err)
	require.NotNil(t, closeFn)
	defer func() { assert.NoError(t, closeFn()) }()

	assert.True(t, database.HasSemantic(),
		"an existing vectors.db must wire a read-only searcher for direct CLI reads")
}
