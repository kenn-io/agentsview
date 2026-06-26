package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
)

func TestResolveDuckDBPushProjects(t *testing.T) {
	tests := []projectResolutionCase[DuckDBPushConfig]{
		{
			name:        "config include used when no flags",
			projects:    []string{"a", "b"},
			wantInclude: []string{"a", "b"},
		},
		{
			name:        "flag include overrides config exclude",
			exclude:     []string{"x"},
			cfg:         DuckDBPushConfig{ProjectsFlag: "a,b"},
			wantInclude: []string{"a", "b"},
		},
		{
			name:     "all-projects clears both",
			projects: []string{"a"},
			cfg:      DuckDBPushConfig{AllProjects: true},
		},
		{
			name:    "both flags is an error",
			cfg:     DuckDBPushConfig{ProjectsFlag: "a", ExcludeProjects: "b"},
			wantErr: true,
		},
		{
			name:    "all-projects with include is an error",
			cfg:     DuckDBPushConfig{AllProjects: true, ProjectsFlag: "a"},
			wantErr: true,
		},
		{
			name:     "config has both projects and exclude is an error",
			projects: []string{"a"},
			exclude:  []string{"x"},
			wantErr:  true,
		},
		{
			name:    "all-projects with exclude is an error",
			cfg:     DuckDBPushConfig{AllProjects: true, ExcludeProjects: "x"},
			wantErr: true,
		},
	}
	runProjectResolutionCases(t, tests,
		func(projects, exclude []string, cfg DuckDBPushConfig) ([]string, []string, error) {
			return resolveDuckDBPushProjects(config.DuckDBConfig{
				Projects:        projects,
				ExcludeProjects: exclude,
			}, cfg)
		},
	)
}

func TestArchiveWriteBackendDuckDBPushPostsToDaemon(t *testing.T) {
	absPath := filepath.Join(t.TempDir(), "agentsview.duckdb")
	ts := duckDBPushDaemonServer(t, wantDuckDBDaemonPush{
		auth:            "Bearer secret",
		full:            true,
		projects:        []string{"a"},
		excludeProjects: []string{"b"},
		path:            absPath,
		machineName:     "workstation",
	}, duckdbsync.PushResult{
		SessionsPushed: 2,
		MessagesPushed: 3,
		Duration:       time.Second,
	})

	backend := newDaemonArchiveWriteBackendForTest(
		config.Config{AuthToken: "secret"}, ts.URL,
	)
	result, err := backend.DuckDBPush(
		context.Background(),
		config.DuckDBConfig{
			Path:        absPath,
			MachineName: "workstation",
		},
		DuckDBPushConfig{Full: true},
		[]string{"a"},
		[]string{"b"},
	)
	require.NoError(t, err)
	assert.Equal(t, 2, result.SessionsPushed)
	assert.Equal(t, 3, result.MessagesPushed)
}

func TestArchiveWriteBackendDuckDBPushAbsolutizesRelativeDaemonPath(t *testing.T) {
	wantPath, err := filepath.Abs("relative.duckdb")
	require.NoError(t, err)
	ts := duckDBPushDaemonServer(t, wantDuckDBDaemonPush{
		path: wantPath,
	}, duckdbsync.PushResult{})

	backend := newDaemonArchiveWriteBackendForTest(config.Config{}, ts.URL)
	_, err = backend.DuckDBPush(
		context.Background(),
		config.DuckDBConfig{Path: "relative.duckdb"},
		DuckDBPushConfig{},
		nil,
		nil,
	)
	require.NoError(t, err)
}

// wantDuckDBDaemonPush is the expected shape of a DuckDB daemon push request.
type wantDuckDBDaemonPush struct {
	auth            string
	full            bool
	projects        []string
	excludeProjects []string
	path            string
	machineName     string
}

// duckDBPushDaemonServer starts a daemon test server on the DuckDB push route
// that asserts the decoded request matches want and replies with result.
func duckDBPushDaemonServer(
	t *testing.T,
	want wantDuckDBDaemonPush,
	result duckdbsync.PushResult,
) *httptest.Server {
	t.Helper()
	return pushRuntimeServer(t, "/api/v1/push/duckdb", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		assert.Equal(t, want.auth, r.Header.Get("Authorization"))
		var req daemonPushRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, want.full, req.Full)
		assert.Equal(t, want.projects, req.Projects)
		assert.Equal(t, want.excludeProjects, req.ExcludeProjects)
		require.NotNil(t, req.DuckDB)
		assert.Equal(t, want.path, req.DuckDB.Path)
		assert.Equal(t, want.machineName, req.DuckDB.MachineName)
		writeTestJSON(t, w, result)
	})
}

func TestResolveQuackServeToken(t *testing.T) {
	generateErr := errors.New("generate failed")
	tests := []struct {
		name       string
		flagToken  string
		configured string
		generated  string
		genErr     error
		wantToken  string
		wantGen    bool
		wantErr    bool
	}{
		{
			name:       "flag token wins",
			flagToken:  "flag-token",
			configured: "config-token",
			generated:  "generated-token",
			wantToken:  "flag-token",
		},
		{
			name:       "configured token used before generation",
			configured: "config-token",
			generated:  "generated-token",
			wantToken:  "config-token",
		},
		{
			name:      "generates token when none configured",
			generated: "generated-token",
			wantToken: "generated-token",
			wantGen:   true,
		},
		{
			name:    "generator error returned",
			genErr:  generateErr,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			token, generated, err := resolveQuackServeToken(
				tt.flagToken, tt.configured,
				func() (string, error) {
					called = true
					return tt.generated, tt.genErr
				},
			)
			if tt.wantErr {
				require.Error(t, err)
				assert.True(t, called)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantToken, token)
			assert.Equal(t, tt.wantGen, generated)
			assert.Equal(t, tt.wantGen, called)
		})
	}
}
