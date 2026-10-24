package server

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// stubVectorPushSource is a no-op storage.VectorPushSource: the gating test
// only needs identity, never a method call.
type stubVectorPushSource struct{}

func TestDaemonPushRequestWatchTransportJSON(t *testing.T) {
	payload := []byte(`{
		"full":false,
		"watch_batch":{"paths":["/sessions/changed.jsonl"],"lost_events":true},
		"watch_recovery":{"available_roots":["/sessions"],"deferred_roots":["/offline"]}
	}`)
	var request daemonPushRequest
	require.NoError(t, json.Unmarshal(payload, &request))
	require.NotNil(t, request.WatchBatch)
	assert.Equal(t, []string{"/sessions/changed.jsonl"}, request.WatchBatch.Paths)
	assert.True(t, request.WatchBatch.LostEvents)
	require.NotNil(t, request.WatchRecovery)
	assert.Equal(t, []string{"/sessions"}, request.WatchRecovery.AvailableRoots)
	assert.Equal(t, []string{"/offline"}, request.WatchRecovery.DeferredRoots)

	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(encoded))
}

func (stubVectorPushSource) BeginExport(
	context.Context, []string,
) (storage.VectorExport, bool, error) {
	return nil, false, nil
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

	logProgress := newReplicaPushProgressLogger("pg")
	logProgress(storage.PushProgress{SessionsDone: 1, SessionsTotal: 10, MessagesDone: 5})
	logProgress(storage.PushProgress{SessionsDone: 2, SessionsTotal: 10, MessagesDone: 9})
	assert.Contains(t, buf.String(), "pg push: 1/10 session(s), 5 messages")
	assert.NotContains(t, buf.String(), "2/10",
		"second report inside the throttle window must not log")

	pushProgressLogInterval = 0
	logProgress(storage.PushProgress{
		Phase:               "vectors",
		VectorSessionsDone:  3,
		VectorSessionsTotal: 7,
		VectorChunksPushed:  42,
	})
	assert.Contains(t, buf.String(),
		"pg push: vectors 3/7 session(s) scanned, 42 chunks")

	logProgress(storage.PushProgress{
		Phase:         "preparing",
		SessionsDone:  500,
		SessionsTotal: 46000,
	})
	assert.Contains(t, buf.String(), "pg push: preparing 500/46000 session(s)")

	logProgress(storage.PushProgress{Phase: "preparing"})
	assert.Contains(t, buf.String(),
		"pg push: preparing (sync state, metadata, fingerprints)",
		"zero-total preparing report renders the setup-stage line")
}

func TestPGPushVectorSourceGating(t *testing.T) {
	tests := []struct {
		name      string
		wired     bool
		optOut    bool
		noVectors bool
		wantSrc   bool
	}{
		{name: "wired and enabled", wired: true, wantSrc: true},
		{name: "no source wired", wired: false, wantSrc: false},
		{name: "target opts out", wired: true, optOut: true, wantSrc: false},
		{name: "caller passed --no-vectors", wired: true, noVectors: true, wantSrc: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{}
			if tt.wired {
				s.vectorPushSource = stubVectorPushSource{}
			}
			got := s.replicaPushVectorSource(
				storage.ReplicaTarget{PushVectors: !tt.optOut}, tt.noVectors,
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

func missingEnvRef(tb testing.TB, name string) string {
	tb.Helper()
	require.NoError(tb, os.Unsetenv(name))
	return "${" + name + "}"
}

func testServerWithConfig(cfg config.Config) *Server {
	return &Server{
		cfg:      cfg,
		replicas: []storage.Replica{postgres.Backend{}, clickhouse.Backend{}},
		mirror:   duckdb.Mirror{},
	}
}

func readOpenAPISpec(tb testing.TB, h http.Handler) openAPISpec {
	tb.Helper()
	req := httptest.NewRequestWithContext(tb.Context(), http.MethodGet, "/api/openapi.json", nil)
	req.Host = "127.0.0.1:0"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	require.Equal(tb, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var spec openAPISpec
	require.NoError(tb, json.Unmarshal(w.Body.Bytes(), &spec))
	return spec
}

func requireOpenAPIOperation(
	tb testing.TB,
	spec openAPISpec,
	method string,
	path string,
) openAPIOperation {
	tb.Helper()
	require.Contains(tb, spec.Paths, path)
	require.Contains(tb, spec.Paths[path], method)
	return spec.Paths[path][method]
}

func assertStreamingResponseContent(
	tb testing.TB,
	content map[string]any,
) {
	tb.Helper()
	assert.Contains(tb, content, "text/event-stream")
	assert.Contains(tb, content, "application/json")
}

func TestPGPushConfigRequestOverrideSkipsDaemonEnvResolution(t *testing.T) {
	const envName = "AGENTSVIEW_TEST_MISSING_PG_URL_25053"
	s := testServerWithConfig(config.Config{
		PG: config.PGConfig{URL: missingEnvRef(t, envName)},
	})
	req := daemonPushRequest{
		Replica: &daemonReplicaTarget{
			URL:         "postgres://user:pass@host/db",
			Schema:      "mirror",
			MachineName: "laptop",
		},
	}

	got, err := s.replicaPushTarget(postgres.Backend{}, req)
	require.NoError(t, err)
	assert.Equal(t, "postgres://user:pass@host/db", got.URL)
	assert.Equal(t, "mirror", got.Schema)
	assert.Equal(t, "laptop", got.MachineName)
}

func TestClickHousePushTargetRequestOverride(t *testing.T) {
	s := testServerWithConfig(config.Config{
		ClickHouse: config.ClickHouseConfig{URL: "clickhouse://from-config"},
	})
	got, err := s.replicaPushTarget(clickhouse.Backend{}, daemonPushRequest{
		Replica: &daemonReplicaTarget{
			URL:         "clickhouse://from-request",
			Schema:      "mirrordb",
			MachineName: "laptop",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "clickhouse://from-request", got.URL)
	assert.Equal(t, "mirrordb", got.Schema)
	assert.Equal(t, "laptop", got.MachineName)
}

func TestClickHousePushTargetDefaultsToDaemonConfig(t *testing.T) {
	s := testServerWithConfig(config.Config{
		ClickHouse: config.ClickHouseConfig{URL: "clickhouse://from-config", Database: "agentsview"},
	})
	got, err := s.replicaPushTarget(clickhouse.Backend{}, daemonPushRequest{})
	require.NoError(t, err)
	assert.Equal(t, "clickhouse://from-config", got.URL)
	assert.Equal(t, "agentsview", got.Schema)
}

func TestClickHousePushRejectsIncludeAndExcludeProjects(t *testing.T) {
	s := testServerWithConfig(config.Config{})
	_, err := s.humaReplicaPush(t.Context(), clickhouse.Backend{}, &daemonPushInput{
		Body: daemonPushRequest{
			Projects:        []string{"alpha"},
			ExcludeProjects: []string{"beta"},
		},
	})
	require.Error(t, err)
	var statusErr interface{ GetStatus() int }
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.GetStatus())
	assert.Contains(t, err.Error(), "projects and exclude_projects are mutually exclusive")
}

func TestPGPushRejectsIncludeAndExcludeProjects(t *testing.T) {
	s := testServerWithConfig(config.Config{})

	_, err := s.humaReplicaPush(t.Context(), postgres.Backend{}, &daemonPushInput{
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
			InputPerMTok:  money.MustParseDollars("2"),
			OutputPerMTok: money.MustParseDollars("8"),
		}}))
		return nil
	}

	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodPost,
		"/api/v1/push/pg",
		strings.NewReader(`{"full":false,"replica":{"url":"postgres://nobody:nobody@127.0.0.1:1/test?sslmode=disable","schema":"agentsview","machine_name":"test","allow_insecure":false}}`),
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
	assert.Equal(t, money.MustParseDollars("8"), rate.OutputPerMTok)
}

func TestDuckDBPushRejectsIncludeAndExcludeProjects(t *testing.T) {
	s := testServerWithConfig(config.Config{})

	_, err := s.humaMirrorPush(t.Context(), &daemonPushInput{
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

	_, err := s.humaMirrorPush(t.Context(), &daemonPushInput{
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
			got, err := s.mirrorPushConfig(daemonPushRequest{DuckDB: tt.req})
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

	_, err := s.humaMirrorPush(t.Context(), &daemonPushInput{
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
	got := mirrorPushOptions(daemonPushRequest{
		Projects:        []string{"alpha"},
		ExcludeProjects: []string{"beta"},
	})

	assert.Equal(t, []string{"alpha"}, got.Projects)
	assert.Equal(t, []string{"beta"}, got.ExcludeProjects)
}

func TestBrowserSyncRoutesAreSSEOnly(t *testing.T) {
	s := testServer(t, 30)
	spec := readOpenAPISpec(t, s.Handler())
	for _, path := range []string{"/api/v1/import/claude-ai/sync", "/api/v1/import/chatgpt/sync"} {
		op := requireOpenAPIOperation(t, spec, "post", path)
		require.Contains(t, op.Responses, "200")
		assert.Len(t, op.Responses["200"].Content, 1)
		assert.Contains(t, op.Responses["200"].Content, "text/event-stream")
	}
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
		req := httptest.NewRequestWithContext(t.Context(),
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
			engine := f.srv.syncEngineForLocal(t.Context(), f.db)
			t.Cleanup(engine.Close)

			err := f.srv.syncThenRunForPush(
				t.Context(), engine, f.db, tt.full, nil, nil, nil,
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
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	t.Cleanup(engine.Close)

	err := f.srv.syncThenRunForPush(
		t.Context(), engine, f.db, true, nil, nil, nil,
		func(bool) error {
			require.FailNow(t, "push work must not run after a failed worker pass")
			return nil
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "resync build reported failed")
}

func TestSyncThenRunForPushCopiesHealthyArchiveBesideCorruptSource(t *testing.T) {
	root := t.TempDir()
	f := newSyncRouteFixture(t, withDisabledAgents(nil, map[parser.AgentType][]string{
		parser.AgentDeepSeekHarness: {root},
	}))
	f.writeClaudeSession(t, "proj/healthy.jsonl", "keep replicating this session")
	dir := filepath.Join(root, "--workspace-example--", "broken")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(`{"type":"session","version":0,"id":"broken","createdAt":1700000000000,"cwd":"/workspace/example","delegationDepth":0,"agentPreset":"coding"}
{"type":"turn/start","seq":0,"time":1700000000001,"data":{"turn":1}}
{"type":"user/message","seq":2,"time":1700000000003,"data":{"id":"user","role":"user","source":{"kind":"user"},"content":[{"type":"text","text":"missing event"}]},"surfaceOp":"append"}
{"type":"turn/end","seq":3,"time":1700000000004,"data":{"turn":1,"status":"completed"}}
`), 0o600))
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	t.Cleanup(engine.Close)
	for _, accept := range []string{"application/json", "text/event-stream"} {
		t.Run(accept, func(t *testing.T) {
			var copied []string
			recorder := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/push/pg", nil)
			request.Header.Set("Accept", accept)
			hctx := humago.NewContext(&huma.Operation{}, request, recorder)
			runPushStream(hctx, func(_, _ func(storage.PushProgress)) (any, error) {
				var result storage.PushResult
				err := f.srv.syncThenRunForPush(t.Context(), engine, f.db, false, nil, nil, nil,
					func(forceFull bool) error {
						assert.False(t, forceFull)
						session, err := f.db.GetSession(t.Context(), "healthy")
						require.NoError(t, err)
						require.NotNil(t, session)
						copied = append(copied, session.ID)
						result = storage.PushResult{
							SessionsPushed: 1, Errors: 2,
							Vectors: storage.VectorPushResult{SessionsDeferred: 3},
						}
						return nil
					})
				return result, err
			})
			assert.Equal(t, []string{"healthy"}, copied)
			require.Equal(t, http.StatusOK, recorder.Code)
			payload := recorder.Body.String()
			if accept == "text/event-stream" {
				require.Contains(t, payload, "event: done\n")
				_, payload, _ = strings.Cut(payload, "data: ")
			}
			var result storage.PushResult
			require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(payload)), &result))
			assert.Equal(t, 1, result.SessionsPushed)
			assert.Equal(t, 2, result.Errors, "row failures must reach the push client")
			assert.Equal(t, 3, result.Vectors.SessionsDeferred)
		})
	}
	pushFailure := errors.New("mirror write failed")
	err := f.srv.syncThenRunForPush(t.Context(), engine, f.db, false, nil, nil, nil,
		func(bool) error { return pushFailure })
	require.ErrorIs(t, err, pushFailure)
	_, notStarted := errors.AsType[storage.PushNotStartedError](err)
	assert.False(t, notStarted, "an error from the push work may follow a started copy")
}

// A daemon push must not block local ingestion: a sync can take the engine
// lock while the push copies the archive.
func TestSyncThenRunForPushLeavesSyncLockFreeDuringPush(t *testing.T) {
	f := newSyncRouteFixture(t)
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	t.Cleanup(engine.Close)

	err := f.srv.syncThenRunForPush(t.Context(), engine, f.db, false, nil, nil, nil,
		func(bool) error {
			return engine.TryRunExclusive(func() error { return nil })
		})

	require.NoError(t, err, "a sync must be able to run while the push runs")
}

// Daemon pushes take turns, so an older push can never overwrite what a newer
// push to the same destination wrote. A push takes its turn before its own
// sync, so its archive rebuild never replaces the archive another push reads.
func TestSyncThenRunForPushRunsOnePushAtATime(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	f := newSyncRouteFixture(t, withLocalResyncRunner(func(
		context.Context, func(syncpkg.Progress),
	) (syncpkg.SyncStats, error) {
		record("second rebuild")
		return syncpkg.SyncStats{}, nil
	}))
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	t.Cleanup(engine.Close)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- f.srv.syncThenRunForPush(t.Context(), engine, f.db, false, nil, nil, nil,
			func(bool) error {
				close(firstStarted)
				<-releaseFirst
				record("first done")
				return nil
			})
	}()
	<-firstStarted
	waiting := make(chan struct{})
	second := make(chan error, 1)
	go func() {
		second <- f.srv.syncThenRunForPush(t.Context(), engine, f.db, true, nil, nil,
			func(phase string) {
				if phase == storage.PushPhaseWaiting {
					close(waiting)
				}
			},
			func(bool) error {
				record("second start")
				return nil
			})
	}()

	<-waiting
	close(releaseFirst)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	assert.Equal(t, []string{"first done", "second rebuild", "second start"}, events)
}

// A push waiting for its turn stops when its request ends.
func TestSyncThenRunForPushStopsWaitingForTurnWhenContextEnds(t *testing.T) {
	f := newSyncRouteFixture(t)
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	t.Cleanup(engine.Close)
	endTurn, err := f.srv.pushTurn.take(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(endTurn)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiting := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.srv.syncThenRunForPush(ctx, engine, f.db, false, nil, nil,
			func(phase string) {
				if phase == storage.PushPhaseWaiting {
					close(waiting)
				}
			},
			func(bool) error {
				assert.Fail(t, "a push must not run while another push holds the turn")
				return nil
			})
	}()
	<-waiting
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestSyncThenRunForPushDeferredWorkerSkipsPush(t *testing.T) {
	f := newSyncRouteFixture(t, withLocalResyncRunner(func(
		context.Context, func(syncpkg.Progress),
	) (syncpkg.SyncStats, error) {
		return syncpkg.SyncStats{Deferred: 1}, nil
	}))
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	t.Cleanup(engine.Close)

	err := f.srv.syncThenRunForPush(
		t.Context(), engine, f.db, true, nil, nil, nil,
		func(bool) error {
			require.FailNow(t, "push work must not run after deferred worker processing")
			return nil
		},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "local sync processing incomplete")
	_, notStarted := errors.AsType[storage.PushNotStartedError](err)
	assert.True(t, notStarted, "a failed pre-push sync never started the push")
}

func TestSyncThenRunForPushAppliesCurrentWatchBatchBeforeWork(t *testing.T) {
	f := newSyncRouteFixture(t)
	changed := f.writeClaudeSession(t, "proj/changed.jsonl", "scoped daemon push")
	untouched := f.writeClaudeSession(t, "proj/untouched.jsonl", "must stay cold")
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	t.Cleanup(engine.Close)
	batch := syncpkg.WatchBatch{Paths: []string{changed}}

	err := f.srv.syncThenRunForPush(
		t.Context(), engine, f.db, false, &batch, nil, nil,
		func(forceFull bool) error {
			assert.False(t, forceFull)
			changedSession, getErr := f.db.GetSession(t.Context(), "changed")
			require.NoError(t, getErr)
			require.NotNil(t, changedSession)
			untouchedSession, getErr := f.db.GetSession(t.Context(), "untouched")
			require.NoError(t, getErr)
			assert.Nil(t, untouchedSession)
			return nil
		},
	)

	require.NoError(t, err)
	assert.FileExists(t, untouched)
}

func TestSyncThenRunForPushStaleArchiveIgnoresBatchAndUsesWorker(t *testing.T) {
	runnerCalls := 0
	f := newSyncRouteFixture(t,
		withStaleDB(),
		withLocalResyncRunner(func(
			context.Context, func(syncpkg.Progress),
		) (syncpkg.SyncStats, error) {
			runnerCalls++
			return syncpkg.SyncStats{}, nil
		}),
	)
	path := f.writeClaudeSession(t, "proj/changed.jsonl", "stale scoped push")
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	t.Cleanup(engine.Close)
	batch := syncpkg.WatchBatch{Paths: []string{path}}
	workCalls := 0

	err := f.srv.syncThenRunForPush(
		t.Context(), engine, f.db, false, &batch, nil, nil,
		func(forceFull bool) error {
			workCalls++
			assert.True(t, forceFull)
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, 1, runnerCalls)
	assert.Equal(t, 1, workCalls)
	assertSessionCount(t, f.db, 0)
}

func TestPGPushRejectsMalformedWatchScopeBeforeSSE(t *testing.T) {
	f := newSyncRouteFixture(t)
	tests := []struct {
		name  string
		body  map[string]any
		match string
	}{
		{
			name: "full batch retains path",
			body: map[string]any{
				"watch_batch": map[string]any{
					"full_sync": true,
					"paths":     []string{"/sessions/changed.jsonl"},
				},
				"watch_recovery": map[string]any{},
			},
			match: "full watch batch",
		},
		{
			name:  "recovery without batch",
			body:  map[string]any{"watch_recovery": map[string]any{}},
			match: "watch recovery requires",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.body["full"] = false
			tt.body["replica"] = map[string]any{
				"url":            "postgres://nobody:nobody@127.0.0.1:1/test?sslmode=disable",
				"schema":         "agentsview",
				"machine_name":   "test",
				"allow_insecure": false,
			}
			w := serveJSON(
				t, f.handler, http.MethodPost, "/api/v1/push/pg", tt.body,
				withAccept("text/event-stream"),
			)
			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			assert.Contains(t, w.Body.String(), tt.match)
			assert.NotContains(t, w.Header().Get("Content-Type"), "text/event-stream")
		})
	}
}

func TestPGPushRejectsWatchPathsOutsideStartupProviderRoots(t *testing.T) {
	f := newSyncRouteFixture(t)
	outside := filepath.Join(t.TempDir(), "session.jsonl")
	for _, tt := range []struct {
		name  string
		body  map[string]any
		match string
	}{
		{
			name: "changed path", match: "watch path",
			body: map[string]any{"watch_batch": map[string]any{
				"paths": []string{outside},
			}},
		},
		{
			name: "UNC changed path", match: "watch path",
			body: map[string]any{"watch_batch": map[string]any{
				"paths": []string{`\\server\share\session.jsonl`},
			}},
		},
		{
			name: "reconciliation root", match: "watch reconciliation root",
			body: map[string]any{"watch_batch": map[string]any{
				"reconcile_roots": []string{outside},
			}},
		},
		{
			name: "rename path", match: "watch rename path",
			body: map[string]any{
				"watch_batch": map[string]any{"renames": []map[string]any{{
					"path": outside, "root": f.claudeDir, "item_type": 1,
				}}},
				"watch_recovery": map[string]any{},
			},
		},
		{
			name: "rename root", match: "watch rename root",
			body: map[string]any{
				"watch_batch": map[string]any{"renames": []map[string]any{{
					"path": filepath.Join(f.claudeDir, "session.jsonl"),
					"root": outside, "item_type": 1,
				}}},
				"watch_recovery": map[string]any{},
			},
		},
		{
			name: "available recovery root", match: "watch recovery root",
			body: map[string]any{
				"watch_batch": map[string]any{"full_sync": true},
				"watch_recovery": map[string]any{
					"available_roots": []string{outside},
				},
			},
		},
		{
			name: "deferred recovery root", match: "watch recovery root",
			body: map[string]any{
				"watch_batch": map[string]any{"full_sync": true},
				"watch_recovery": map[string]any{
					"deferred_roots": []string{outside},
				},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.body["full"] = false
			tt.body["replica"] = map[string]any{
				"url":            "postgres://nobody:nobody@127.0.0.1:1/test?sslmode=disable",
				"schema":         "agentsview",
				"machine_name":   "test",
				"allow_insecure": false,
			}
			w := serveJSON(
				t, f.handler, http.MethodPost, "/api/v1/push/pg",
				tt.body,
				withAccept("text/event-stream"),
			)

			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
			assert.Contains(t, w.Body.String(), tt.match)
			assert.NotContains(t, w.Header().Get("Content-Type"), "text/event-stream")
		})
	}
}

func TestValidatePushWatchScopeAcceptsConfiguredAndProviderWatchRoots(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	index := filepath.Join(filepath.Dir(root), parser.CodexSessionIndexFilename)
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentCodex: {root},
	}}

	require.NoError(t, validatePushWatchScope(t.Context(), daemonPushRequest{
		WatchBatch: &syncpkg.WatchBatch{Paths: []string{
			filepath.Join(root, "2026", "session.jsonl"), index,
		}},
	}, cfg))
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
			"replica": map[string]any{
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

func TestValidatePushWatchScopeAcceptsAliasIndexWithoutEngine(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "primary", "sessions")
	alias := filepath.Join(base, "profile", "sessions")
	cfg := config.Config{
		NoSync:    true,
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
		ProviderMetadata: map[parser.AgentType]map[string][]string{
			parser.AgentCodex: {root: {filepath.Dir(root), filepath.Dir(alias)}},
		},
	}
	require.NoError(t, validatePushWatchScope(t.Context(), daemonPushRequest{
		WatchBatch: &syncpkg.WatchBatch{Paths: []string{
			filepath.Join(base, "profile", parser.CodexSessionIndexFilename),
		}},
	}, cfg))
}

// TestReplicaPushTargetOmittedPushVectorsDefaultsOn pins the wire default:
// a delegated push that leaves push_vectors out keeps the vector phase on,
// matching the [pg] config default, while an explicit false still opts out.
func TestReplicaPushTargetOmittedPushVectorsDefaultsOn(t *testing.T) {
	s := testServerWithConfig(config.Config{})
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{name: "omitted", body: `{"url":"postgres://h/db","machine_name":"m"}`, want: true},
		{name: "explicit true", body: `{"url":"postgres://h/db","machine_name":"m","push_vectors":true}`, want: true},
		{name: "explicit false", body: `{"url":"postgres://h/db","machine_name":"m","push_vectors":false}`, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var wire daemonReplicaTarget
			require.NoError(t, json.Unmarshal([]byte(tt.body), &wire))
			got, err := s.replicaPushTarget(postgres.Backend{}, daemonPushRequest{Replica: &wire})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.PushVectors)
		})
	}
}

// TestReplicaPushTargetCarriesPushBatchSize pins that a delegated push hands
// the target's push_batch_size to the pusher, and that omitting it selects
// the push default.
func TestReplicaPushTargetCarriesPushBatchSize(t *testing.T) {
	s := testServerWithConfig(config.Config{})
	for _, tt := range []struct {
		name string
		body string
		want int
	}{
		{name: "omitted", body: `{"url":"clickhouse://localhost/db","machine_name":"m"}`, want: 0},
		{name: "set", body: `{"url":"clickhouse://localhost/db","machine_name":"m","push_batch_size":1000}`, want: 1000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var wire daemonReplicaTarget
			require.NoError(t, json.Unmarshal([]byte(tt.body), &wire))
			got, err := s.replicaPushTarget(clickhouse.Backend{}, daemonPushRequest{Replica: &wire})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.PushBatchSize)
		})
	}
}

// TestClickHousePushRejectsPlaintextTargetBeforeStream pins that a ClickHouse
// target the backend refuses (plaintext to a non-loopback host without
// allow_insecure) fails with a 400 from the handler, before the stream opens
// and before any local sync pass runs.
func TestClickHousePushRejectsPlaintextTargetBeforeStream(t *testing.T) {
	s := testServer(t, 30*time.Second)
	_, err := s.humaReplicaPush(t.Context(), clickhouse.Backend{}, &daemonPushInput{
		Body: daemonPushRequest{
			Replica: &daemonReplicaTarget{
				URL:         "clickhouse://user:pw@ch.example.test:9000/agentsview",
				Schema:      "agentsview",
				MachineName: "laptop",
			},
		},
	})
	require.Error(t, err)
	var statusErr interface{ GetStatus() int }
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.GetStatus())
	assert.Contains(t, err.Error(), "allow_insecure")
}

// A daemon push that waits for another push tells its client, so the CLI can
// say why nothing is happening.
func TestMirrorPushStreamsWaitingForAnotherPush(t *testing.T) {
	f := newSyncRouteFixture(t)
	f.srv.cfg.DuckDB = config.DuckDBConfig{
		Path:        filepath.Join(t.TempDir(), "mirror.duckdb"),
		MachineName: "daemon",
	}
	endTurn, err := f.srv.pushTurn.take(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(endTurn)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := f.srv.humaMirrorPush(ctx, &daemonPushInput{})
	require.NoError(t, err)
	writer := newSignalingRecorder(`"Phase":"waiting"`)
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/push/duckdb", nil)
	request.Header.Set("Accept", "text/event-stream")
	done := make(chan struct{})
	go func() {
		defer close(done)
		stream.Body(humago.NewContext(&huma.Operation{}, request, writer))
	}()

	<-writer.matched
	cancel()
	<-done
	assert.Contains(t, writer.String(), "event: progress\n")
}

// A daemon push whose sync rebuilt the archive reports it in the stream before
// the result and marks the mirror result. A push that rebuilt nothing reports
// neither, so a client whose stream drops keeps its --full request.
func TestMirrorPushReportsArchiveRebuild(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("rebuild=%t", rebuild), func(t *testing.T) {
			var f *syncRouteFixture
			f = newSyncRouteFixture(t, withLocalResyncRunner(func(
				ctx context.Context, _ func(syncpkg.Progress),
			) (syncpkg.SyncStats, error) {
				return syncpkg.SyncStats{}, f.db.SetDatabaseIDForTest(ctx, "rebuilt")
			}))
			f.srv.cfg.DuckDB = config.DuckDBConfig{
				Path:        filepath.Join(t.TempDir(), "mirror.duckdb"),
				MachineName: "daemon",
			}
			// Only a full push routes its sync through the rebuilding runner.
			stream, err := f.srv.humaMirrorPush(t.Context(),
				&daemonPushInput{Body: daemonPushRequest{Full: rebuild}})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/push/duckdb", nil)
			request.Header.Set("Accept", "text/event-stream")
			stream.Body(humago.NewContext(&huma.Operation{}, request, recorder))

			body := recorder.Body.String()
			reported := strings.Index(body, `"Phase":"rebuilt"`)
			finished := strings.Index(body, "event: done\n")
			require.NotEqual(t, -1, finished, "push did not finish: %s", body)
			var result storage.MirrorPushResult
			require.NoError(t, json.Unmarshal(
				[]byte(strings.TrimSpace(strings.TrimPrefix(body[finished:], "event: done\ndata: "))),
				&result,
			))
			assert.Equal(t, rebuild, result.ArchiveRebuilt)
			if !rebuild {
				assert.Equal(t, -1, reported, "stream reports a rebuild: %s", body)
				return
			}
			require.NotEqual(t, -1, reported, "stream has no rebuild report: %s", body)
			assert.Less(t, reported, finished)
		})
	}
}

// signalingRecorder is a goroutine-safe flushing ResponseWriter that closes
// matched once its body contains want.
type signalingRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	want    string
	matched chan struct{}
	once    sync.Once
}

func newSignalingRecorder(want string) *signalingRecorder {
	return &signalingRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		want:             want,
		matched:          make(chan struct{}),
	}
}

func (r *signalingRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.ResponseRecorder.Write(p)
	if strings.Contains(r.Body.String(), r.want) {
		r.once.Do(func() { close(r.matched) })
	}
	return n, err
}

func (r *signalingRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ResponseRecorder.Flush()
}

func (r *signalingRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Body.String()
}

// A push error tells the client whether the copy started, in both response
// shapes, so a watch can keep a --full request that no push started.
func TestRunPushStreamReportsWhetherThePushStarted(t *testing.T) {
	for _, accept := range []string{"application/json", "text/event-stream"} {
		for _, tt := range []struct {
			err     error
			started bool
		}{
			{storage.PushNotStartedError{Err: errors.New("resync failed")}, false},
			{errors.New("copy failed"), true},
		} {
			t.Run(fmt.Sprintf("%s started=%t", accept, tt.started), func(t *testing.T) {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/push/pg", nil)
				request.Header.Set("Accept", accept)
				runPushStream(humago.NewContext(&huma.Operation{}, request, recorder),
					func(_, _ func(storage.PushProgress)) (any, error) { return nil, tt.err })
				payload := recorder.Body.String()
				if accept == "text/event-stream" {
					require.Contains(t, payload, "event: error\n")
					_, payload, _ = strings.Cut(payload, "data: ")
				}
				var body pushErrorResponse
				require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(payload)), &body))
				assert.Equal(t, tt.err.Error(), body.Error)
				assert.Equal(t, tt.started, body.PushStarted)
			})
		}
	}
}

// stubReplica is a PostgreSQL replica whose pusher the test supplies.
type stubReplica struct {
	postgres.Backend
	newPusher func() (storage.Pusher, error)
}

func (r stubReplica) NewPusher(
	context.Context, storage.ReplicaTarget, *db.DB, storage.PusherOptions,
) (storage.Pusher, error) {
	return r.newPusher()
}

type failingPusher struct{ err error }

func (failingPusher) EnsureSchema(context.Context) error { return nil }
func (p failingPusher) PushWithOptions(
	context.Context, storage.PushOptions, func(storage.PushProgress),
) (storage.PushResult, error) {
	return storage.PushResult{}, p.err
}
func (failingPusher) Close() error { return nil }

// The replica route reports whether the copy started: a failed connect
// started nothing, and a failed copy may have recorded a full push the mirror
// resumes.
func TestReplicaPushReportsWhetherTheCopyStarted(t *testing.T) {
	f := newSyncRouteFixture(t)
	for _, tt := range []struct {
		name      string
		newPusher func() (storage.Pusher, error)
		started   bool
	}{
		{"connect failed", func() (storage.Pusher, error) {
			return nil, errors.New("connection refused")
		}, false},
		{"copy failed", func() (storage.Pusher, error) {
			return failingPusher{errors.New("conn reset")}, nil
		}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stream, err := f.srv.humaReplicaPush(t.Context(),
				stubReplica{newPusher: tt.newPusher},
				&daemonPushInput{Body: daemonPushRequest{Replica: &daemonReplicaTarget{
					URL: "postgres://127.0.0.1:5432/agentsview?sslmode=disable", MachineName: "test",
				}}})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/push/pg", nil)
			request.Header.Set("Accept", "text/event-stream")
			stream.Body(humago.NewContext(&huma.Operation{}, request, recorder))

			_, payload, found := strings.Cut(recorder.Body.String(), "event: error\ndata: ")
			require.True(t, found, recorder.Body.String())
			var body pushErrorResponse
			require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(payload)), &body))
			assert.Equal(t, tt.started, body.PushStarted)
		})
	}
}

// A push that rebuilt the archive and then failed to connect reports the push
// as started. Every mirror pushes in full after a rebuild, and a client that
// kept --full would make the daemon rebuild the archive on every retry.
func TestSyncThenRunForPushCountsARebuildAsStarted(t *testing.T) {
	var f *syncRouteFixture
	f = newSyncRouteFixture(t, withLocalResyncRunner(func(
		ctx context.Context, _ func(syncpkg.Progress),
	) (syncpkg.SyncStats, error) {
		return syncpkg.SyncStats{}, f.db.SetDatabaseIDForTest(ctx, "rebuilt")
	}))
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	t.Cleanup(engine.Close)
	connectErr := storage.PushNotStartedError{Err: errors.New("connection refused")}

	for _, full := range []bool{false, true} {
		err := f.srv.syncThenRunForPush(t.Context(), engine, f.db, full, nil, nil, nil,
			func(bool) error { return connectErr })
		require.ErrorIs(t, err, connectErr.Err)
		assert.Equal(t, full, pushErrorBody(err.Error(), err).PushStarted,
			"full=%t: only a push that rebuilt the archive counts as started", full)
	}
}
