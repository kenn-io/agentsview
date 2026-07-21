package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/postgres"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// stubVectorPushSource is a no-op postgres.VectorPushSource: the gating test
// only needs identity, never a method call.
type stubVectorPushSource struct{}

func (stubVectorPushSource) Generation(
	context.Context,
) (postgres.VectorGenerationInfo, bool, error) {
	return postgres.VectorGenerationInfo{}, false, nil
}

func (stubVectorPushSource) SessionDocHashes(
	context.Context, []string,
) (map[string]string, error) {
	return nil, nil
}

func (stubVectorPushSource) SessionDocs(
	context.Context, string,
) ([]postgres.VectorPushDoc, string, error) {
	return nil, "", nil
}

// TestPGPushProgressLoggerThrottlesAndReportsPhases pins the daemon-side
// push heartbeat: reports inside the throttle window log nothing, and the
// vector phase logs its own counters instead of the session line.
func TestPGPushProgressLoggerThrottlesAndReportsPhases(t *testing.T) {
	origInterval := pushProgressLogInterval
	pushProgressLogInterval = time.Hour
	t.Cleanup(func() { pushProgressLogInterval = origInterval })

	var buf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(origOut) })

	logProgress := newPGPushProgressLogger()
	logProgress(postgres.PushProgress{SessionsDone: 1, SessionsTotal: 10, MessagesDone: 5})
	logProgress(postgres.PushProgress{SessionsDone: 2, SessionsTotal: 10, MessagesDone: 9})
	assert.Contains(t, buf.String(), "pg push: 1/10 session(s), 5 messages")
	assert.NotContains(t, buf.String(), "2/10",
		"second report inside the throttle window must not log")

	pushProgressLogInterval = 0
	logProgress(postgres.PushProgress{
		Phase:               "vectors",
		VectorSessionsDone:  3,
		VectorSessionsTotal: 7,
		VectorChunksPushed:  42,
	})
	assert.Contains(t, buf.String(),
		"pg push: vectors 3/7 session(s) scanned, 42 chunks")

	logProgress(postgres.PushProgress{
		Phase:         "preparing",
		SessionsDone:  500,
		SessionsTotal: 46000,
	})
	assert.Contains(t, buf.String(), "pg push: preparing 500/46000 session(s)")

	logProgress(postgres.PushProgress{Phase: "preparing"})
	assert.Contains(t, buf.String(),
		"pg push: preparing (sync state, metadata, fingerprints)",
		"zero-total preparing report renders the setup-stage line")
}

func TestPGPushVectorSourceGating(t *testing.T) {
	disabled := false
	tests := []struct {
		name      string
		wired     bool
		pushFlag  *bool
		noVectors bool
		wantSrc   bool
	}{
		{name: "wired and enabled", wired: true, wantSrc: true},
		{name: "no source wired", wired: false, wantSrc: false},
		{name: "target opts out", wired: true, pushFlag: &disabled, wantSrc: false},
		{name: "caller passed --no-vectors", wired: true, noVectors: true, wantSrc: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			if tt.wired {
				s.vectorPushSource = stubVectorPushSource{}
			}
			got := s.pgPushVectorSource(
				config.PGConfig{PushVectors: tt.pushFlag}, tt.noVectors,
			)
			if tt.wantSrc {
				assert.NotNil(t, got)
			} else {
				assert.Nil(t, got)
			}
		})
	}
}

type openAPISpec struct {
	Paths map[string]map[string]openAPIOperation `json:"paths"`
}

type openAPIOperation struct {
	Parameters []openAPIParameter         `json:"parameters"`
	Responses  map[string]openAPIResponse `json:"responses"`
}

type openAPIParameter struct {
	In       string `json:"in"`
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

type openAPIResponse struct {
	Content map[string]any `json:"content"`
}

func missingEnvRef(t testing.TB, name string) string {
	t.Helper()
	require.NoError(t, os.Unsetenv(name))
	return "${" + name + "}"
}

func testServerWithConfig(cfg config.Config) *Server {
	return &Server{cfg: cfg}
}

func readOpenAPISpec(t testing.TB, h http.Handler) openAPISpec {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil)
	req.Host = "127.0.0.1:0"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var spec openAPISpec
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &spec))
	return spec
}

func requireOpenAPIOperation(
	t testing.TB,
	spec openAPISpec,
	method string,
	path string,
) openAPIOperation {
	t.Helper()
	require.Contains(t, spec.Paths, path)
	require.Contains(t, spec.Paths[path], method)
	return spec.Paths[path][method]
}

func assertStreamingResponseContent(
	t testing.TB,
	content map[string]any,
) {
	t.Helper()
	assert.Contains(t, content, "text/event-stream")
	assert.Contains(t, content, "application/json")
}

func TestPGPushConfigRequestOverrideSkipsDaemonEnvResolution(t *testing.T) {
	const envName = "AGENTSVIEW_TEST_MISSING_PG_URL_25053"
	s := testServerWithConfig(config.Config{
		PG: config.PGConfig{URL: missingEnvRef(t, envName)},
	})
	req := daemonPushRequest{
		PG: &config.PGConfig{
			URL:         "postgres://user:pass@host/db",
			Schema:      "mirror",
			MachineName: "laptop",
		},
	}

	got, err := s.pgPushConfig(req)
	require.NoError(t, err)
	assert.Equal(t, "postgres://user:pass@host/db", got.URL)
	assert.Equal(t, "mirror", got.Schema)
	assert.Equal(t, "laptop", got.MachineName)
}

func TestPGPushRejectsIncludeAndExcludeProjects(t *testing.T) {
	s := testServerWithConfig(config.Config{})

	_, err := s.humaPGPush(context.Background(), &daemonPushInput{
		Body: daemonPushRequest{
			Projects:        []string{"alpha"},
			ExcludeProjects: []string{"beta"},
		},
	})
	require.Error(t, err)

	var statusErr interface{ GetStatus() int }
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.GetStatus())
	assert.Contains(t, err.Error(),
		"projects and exclude_projects are mutually exclusive")
}

func TestPGPushEnsuresPricingAfterLocalSync(t *testing.T) {
	s := testServer(t, 30*time.Second)
	database := s.db.(*db.DB)
	s.ensurePricing = func(_ context.Context, got *db.DB) error {
		require.Same(t, database, got)
		require.NoError(t, got.UpsertModelPricing([]db.ModelPricing{{
			ModelPattern:  "new-model",
			InputPerMTok:  2,
			OutputPerMTok: 8,
		}}))
		return nil
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/push/pg",
		strings.NewReader(`{"full":false,"pg":{"url":"postgres://nobody:nobody@127.0.0.1:1/test?sslmode=disable","schema":"agentsview","machine_name":"test","allow_insecure":false}}`),
	)
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"body: %s", w.Body.String())
	rate, err := database.GetModelPricing("new-model")
	require.NoError(t, err)
	require.NotNil(t, rate)
	assert.Equal(t, 8.0, rate.OutputPerMTok)
}

func TestDuckDBPushRejectsIncludeAndExcludeProjects(t *testing.T) {
	s := testServerWithConfig(config.Config{})

	_, err := s.humaDuckDBPush(context.Background(), &daemonPushInput{
		Body: daemonPushRequest{
			Projects:        []string{"alpha"},
			ExcludeProjects: []string{"beta"},
		},
	})
	require.Error(t, err)

	var statusErr interface{ GetStatus() int }
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.GetStatus())
	assert.Contains(t, err.Error(),
		"projects and exclude_projects are mutually exclusive")
}

// TestDuckDBPushRejectsRemoteURLAsBadRequest verifies the daemon-side push
// route rejects a remote Quack URL as bad request: push writes the local
// mirror only, so a configured [duckdb].url is never a valid push target.
func TestDuckDBPushRejectsRemoteURLAsBadRequest(t *testing.T) {
	s := testServer(t, 30)

	_, err := s.humaDuckDBPush(context.Background(), &daemonPushInput{
		Body: daemonPushRequest{
			DuckDB: &config.DuckDBConfig{
				URL:         "quack:https://duck.example.test",
				MachineName: "workstation",
			},
		},
	})
	require.Error(t, err)

	var statusErr interface{ GetStatus() int }
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.GetStatus())
	assert.Contains(t, err.Error(), "duckdb push writes the local mirror")
}

// TestDuckDBPushConfigPinsServerMirrorPath pins the daemon-side path
// guard: the mirror path a push writes is always the server's own resolved
// configuration. A request-supplied config may still carry non-path fields
// (machine name), but a request naming a DIFFERENT path is rejected — an
// authenticated API caller must not be able to aim the rebuild's atomic
// file replacement at an arbitrary daemon-writable file such as the
// primary sessions.db.
func TestDuckDBPushConfigPinsServerMirrorPath(t *testing.T) {
	serverPath := filepath.Join(t.TempDir(), "server.duckdb")
	s := testServerWithConfig(config.Config{
		DuckDB: config.DuckDBConfig{Path: serverPath, MachineName: "daemon"},
	})

	tests := []struct {
		name        string
		req         *config.DuckDBConfig
		wantMachine string
		wantErrHas  string
	}{
		{
			name:        "nil request config uses server config",
			req:         nil,
			wantMachine: "daemon",
		},
		{
			name:        "empty request path defers to server path",
			req:         &config.DuckDBConfig{MachineName: "workstation"},
			wantMachine: "workstation",
		},
		{
			name: "equal path in unclean form is accepted",
			req: &config.DuckDBConfig{
				Path: filepath.Join(
					filepath.Dir(serverPath), ".", filepath.Base(serverPath),
				),
				MachineName: "workstation",
			},
			wantMachine: "workstation",
		},
		{
			name: "different path is rejected",
			req: &config.DuckDBConfig{
				Path:        filepath.Join(t.TempDir(), "sessions.db"),
				MachineName: "workstation",
			},
			wantErrHas: "server-configured mirror path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.duckDBPushConfig(daemonPushRequest{DuckDB: tt.req})
			if tt.wantErrHas != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrHas)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, serverPath, got.Path,
				"pushes must always write the server-resolved mirror path")
			assert.Equal(t, tt.wantMachine, got.MachineName)
		})
	}
}

// TestDuckDBPushRejectsMismatchedMirrorPathAsBadRequest is the handler-level
// twin of TestDuckDBPushConfigPinsServerMirrorPath: the route surfaces the
// path mismatch as a 400 instead of writing anywhere.
func TestDuckDBPushRejectsMismatchedMirrorPathAsBadRequest(t *testing.T) {
	s := testServer(t, 30)
	s.cfg.DuckDB = config.DuckDBConfig{
		Path:        filepath.Join(t.TempDir(), "server.duckdb"),
		MachineName: "daemon",
	}

	_, err := s.humaDuckDBPush(context.Background(), &daemonPushInput{
		Body: daemonPushRequest{
			DuckDB: &config.DuckDBConfig{
				Path:        filepath.Join(t.TempDir(), "sessions.db"),
				MachineName: "workstation",
			},
		},
	})
	require.Error(t, err)

	var statusErr interface{ GetStatus() int }
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.GetStatus())
	assert.Contains(t, err.Error(), "server-configured mirror path")
}

func TestDuckDBPushSyncOptionsPassesThroughProjectFilters(t *testing.T) {
	got := duckDBPushSyncOptions(daemonPushRequest{
		Projects:        []string{"alpha"},
		ExcludeProjects: []string{"beta"},
	})

	assert.Equal(t, []string{"alpha"}, got.Projects)
	assert.Equal(t, []string{"beta"}, got.ExcludeProjects)
}

func TestSyncRemotesRouteIsStreaming(t *testing.T) {
	s := testServer(t, 30)
	spec := readOpenAPISpec(t, s.Handler())
	op := requireOpenAPIOperation(t, spec, "post", "/api/v1/sync/remotes")
	require.Contains(t, op.Responses, "200")
	assertStreamingResponseContent(t, op.Responses["200"].Content)
}

// TestPushRoutesAreStreaming pins that both push routes negotiate SSE (the
// CLI's daemon-delegated push renders the streamed progress) while still
// declaring a plain JSON response for non-streaming clients.
func TestPushRoutesAreStreaming(t *testing.T) {
	s := testServer(t, 30)
	spec := readOpenAPISpec(t, s.Handler())
	for _, path := range []string{"/api/v1/push/pg", "/api/v1/push/duckdb"} {
		op := requireOpenAPIOperation(t, spec, "post", path)
		require.Contains(t, op.Responses, "200", path)
		assertStreamingResponseContent(t, op.Responses["200"].Content)
	}
}

// TestPushRoutesReturn503WhileWriterClosedForSSE pins that a push during the
// write barrier is rejected before the stream body flushes a 200: the daemon
// CLI always negotiates SSE, so the 503 + Retry-After must be decided up
// front rather than emitted as a generic SSE error event.
func TestPushRoutesReturn503WhileWriterClosedForSSE(t *testing.T) {
	s := testServer(t, 30*time.Second)
	database := s.db.(*db.DB)
	require.NoError(t, database.CloseWriter())
	defer func() { assert.NoError(t, database.ReopenWriter()) }()

	for _, path := range []string{"/api/v1/push/pg", "/api/v1/push/duckdb"} {
		req := httptest.NewRequest(
			http.MethodPost, path, strings.NewReader(`{"full":false}`),
		)
		req.Host = "127.0.0.1:0"
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Origin", "http://127.0.0.1:0")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)

		require.Equal(t, http.StatusServiceUnavailable, w.Code,
			"%s body: %s", path, w.Body.String())
		assert.Equal(t, "5", w.Header().Get("Retry-After"),
			"%s: a writer-closed push must advertise Retry-After", path)
	}
}

// TestSyncThenRunForPushWorkerRunnerRouting pins the daemon push coordinator:
// with the worker-backed resync runner wired, full and stale-archive pushes
// run the worker build-and-swap instead of an in-process archive-scale pass
// (the on-disk session must NOT land in the archive, proving no direct
// sync/resync ran in process), and the push work still runs afterwards with a
// full push forced. Per-batch pushes and runner-less servers keep the
// in-process SyncThenRun path, whose sync does land the session.
func TestSyncThenRunForPushWorkerRunnerRouting(t *testing.T) {
	tests := []struct {
		name          string
		stale         bool
		full          bool
		wireRunner    bool
		wantRunner    int
		wantForceFull bool
		wantSessions  int
	}{
		{
			name:          "full push routes through worker resync runner",
			full:          true,
			wireRunner:    true,
			wantRunner:    1,
			wantForceFull: true,
			wantSessions:  0,
		},
		{
			name:          "stale archive push routes through worker resync runner",
			stale:         true,
			wireRunner:    true,
			wantRunner:    1,
			wantForceFull: true,
			wantSessions:  0,
		},
		{
			name:         "per-batch push stays in process",
			wireRunner:   true,
			wantRunner:   0,
			wantSessions: 1,
		},
		{
			name:          "full push without runner falls back in process",
			full:          true,
			wantForceFull: true,
			wantSessions:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runnerCalls := 0
			workCalls := 0
			var opts []syncRouteFixtureOption
			if tt.stale {
				opts = append(opts, withStaleDB())
			}
			if tt.wireRunner {
				opts = append(opts, withLocalResyncRunner(func(
					context.Context, func(syncpkg.Progress),
				) (syncpkg.SyncStats, error) {
					runnerCalls++
					assert.Zero(t, workCalls,
						"the worker pass must complete before the push runs")
					return syncpkg.SyncStats{}, nil
				}))
			}
			f := newSyncRouteFixture(t, opts...)
			f.writeClaudeSession(t, "proj/session.jsonl", "push coordinator")
			engine := f.srv.syncEngineForLocal(f.db)
			t.Cleanup(engine.Close)

			err := f.srv.syncThenRunForPush(
				context.Background(), engine, f.db, tt.full,
				func(forceFull bool) error {
					workCalls++
					assert.Equal(t, tt.wantForceFull, forceFull)
					return nil
				},
			)

			require.NoError(t, err)
			assert.Equal(t, tt.wantRunner, runnerCalls)
			assert.Equal(t, 1, workCalls, "push work must run exactly once")
			assertSessionCount(t, f.db, tt.wantSessions)
		})
	}
}

// TestSyncThenRunForPushRunnerErrorSkipsPush pins the failure contract: a
// worker resync pass that ran and reported failure surfaces its error and the
// push never runs against the unrebuilt archive.
func TestSyncThenRunForPushRunnerErrorSkipsPush(t *testing.T) {
	f := newSyncRouteFixture(t, withLocalResyncRunner(func(
		context.Context, func(syncpkg.Progress),
	) (syncpkg.SyncStats, error) {
		return syncpkg.SyncStats{}, errors.New("resync build reported failed")
	}))
	engine := f.srv.syncEngineForLocal(f.db)
	t.Cleanup(engine.Close)

	err := f.srv.syncThenRunForPush(
		context.Background(), engine, f.db, true,
		func(bool) error {
			require.FailNow(t, "push work must not run after a failed worker pass")
			return nil
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "resync build reported failed")
}

// TestPGPushFullRoutesResyncThroughWorkerRunner is the handler-level twin of
// the coordinator table test: a full pg push over HTTP invokes the wired
// worker resync runner and never runs the archive-scale pass in process (the
// on-disk session must not land in the archive).
func TestPGPushFullRoutesResyncThroughWorkerRunner(t *testing.T) {
	runnerCalls := 0
	f := newSyncRouteFixture(t, withLocalResyncRunner(func(
		context.Context, func(syncpkg.Progress),
	) (syncpkg.SyncStats, error) {
		runnerCalls++
		return syncpkg.SyncStats{}, nil
	}))
	f.writeClaudeSession(t, "proj/session.jsonl", "full push worker routing")
	f.srv.ensurePricing = func(context.Context, *db.DB) error { return nil }

	w := serveJSON(t, f.handler, http.MethodPost, "/api/v1/push/pg",
		map[string]any{
			"full": true,
			"pg": map[string]any{
				"url":            "postgres://nobody:nobody@127.0.0.1:1/test?sslmode=disable",
				"schema":         "agentsview",
				"machine_name":   "test",
				"allow_insecure": false,
			},
		})

	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"the push itself fails against the unreachable target; body: %s",
		w.Body.String())
	assert.Equal(t, 1, runnerCalls,
		"a full push must run the worker-backed resync pass")
	assertSessionCount(t, f.db, 0)
}

// TestNewPushProgressStreamSenderThrottles pins the SSE fan-out throttle: the
// session loop reports per session, and forwarding every report would emit
// one SSE event per row.
func TestNewPushProgressStreamSenderThrottles(t *testing.T) {
	origInterval := pushProgressStreamInterval
	pushProgressStreamInterval = time.Hour
	t.Cleanup(func() { pushProgressStreamInterval = origInterval })

	var got []int
	send := newPushProgressStreamSender(func(v int) { got = append(got, v) })
	send(1)
	send(2)
	assert.Equal(t, []int{1}, got,
		"second report inside the throttle window must not send")

	pushProgressStreamInterval = 0
	send(3)
	assert.Equal(t, []int{1, 3}, got)
}
