package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

type openAPISpec struct {
	Paths map[string]map[string]openAPIOperation `json:"paths"`
}

type openAPIOperation struct {
	Responses map[string]openAPIResponse `json:"responses"`
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

func TestSyncRemotesRouteIsStreaming(t *testing.T) {
	s := testServer(t, 30)
	spec := readOpenAPISpec(t, s.Handler())
	op := requireOpenAPIOperation(t, spec, "post", "/api/v1/sync/remotes")
	require.Contains(t, op.Responses, "200")
	assertStreamingResponseContent(t, op.Responses["200"].Content)
}
