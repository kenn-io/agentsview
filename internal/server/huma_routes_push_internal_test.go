package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/postgres"
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
	context.Context,
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

func TestDuckDBPushRejectsMissingQuackTokenAsBadRequest(t *testing.T) {
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
	assert.Contains(t, err.Error(), "duckdb quack token is required")
}

func TestDuckDBPushConfigRequestOverrideSkipsDaemonEnvResolution(t *testing.T) {
	const envName = "AGENTSVIEW_TEST_MISSING_DUCKDB_PATH_25053"
	s := testServerWithConfig(config.Config{
		DuckDB: config.DuckDBConfig{Path: missingEnvRef(t, envName)},
	})
	req := daemonPushRequest{
		DuckDB: &config.DuckDBConfig{
			Path:        "/tmp/agentsview.duckdb",
			MachineName: "workstation",
		},
	}

	got, err := s.duckDBPushConfig(req)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/agentsview.duckdb", got.Path)
	assert.Equal(t, "workstation", got.MachineName)
}

func TestDuckDBPushSyncOptionsDerivesRemoteTargetScope(t *testing.T) {
	duckCfg := config.DuckDBConfig{
		URL:   "quack:https://duck.example.test?token=secret",
		Token: "other-secret",
	}

	got := duckDBPushSyncOptions(daemonPushRequest{
		Projects: []string{"alpha"},
	}, duckCfg)

	assert.Equal(t, []string{"alpha"}, got.Projects)
	assert.Equal(t, duckdbsync.SyncStateTargetForConfig(duckCfg), got.SyncStateTarget)
	assert.NotContains(t, got.SyncStateTarget, "secret")
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
