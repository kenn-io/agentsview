package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

// projectResolutionCase is a shared table-test row for the PG and DuckDB push
// project resolvers, which apply identical include/exclude precedence rules.
type projectResolutionCase[C any] struct {
	name        string
	projects    []string
	exclude     []string
	cfg         C
	wantInclude []string
	wantExclude []string
	wantErr     bool
}

// runProjectResolutionCases drives the shared project-resolution table against
// resolve, which adapts the configured projects/exclude lists and push config
// into the backend-specific resolver so PG and DuckDB stay in sync.
func runProjectResolutionCases[C any](
	t *testing.T,
	tests []projectResolutionCase[C],
	resolve func(projects, exclude []string, cfg C) ([]string, []string, error),
) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inc, exc, err := resolve(tt.projects, tt.exclude, tt.cfg)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantInclude, inc)
			assert.Equal(t, tt.wantExclude, exc)
		})
	}
}

// pushRuntimeServer starts a daemon test server that serves a single push route
// at pushPath in addition to the standard daemon ping probe.
func pushRuntimeServer(
	t *testing.T,
	pushPath string,
	pushHandler http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	return daemonRouteTestServer(t, map[string]http.HandlerFunc{
		pushPath: pushHandler,
	})
}

// writeTestJSON sets the JSON content type and encodes value as the response
// body, failing the test on encode errors.
func writeTestJSON[T any](t *testing.T, w http.ResponseWriter, value T) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(value))
}

// newDaemonArchiveWriteBackendForTest builds a daemon-backed archive write
// backend that talks HTTP to url.
func newDaemonArchiveWriteBackendForTest(
	appCfg config.Config,
	url string,
) daemonArchiveWriteBackend {
	return daemonArchiveWriteBackend{
		appCfg: appCfg,
		tr:     transport{Mode: transportHTTP, URL: url},
	}
}
