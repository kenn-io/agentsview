package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
)

func TestResolveDuckDBPushProjects(t *testing.T) {
	tests := []struct {
		name        string
		duck        config.DuckDBConfig
		cfg         DuckDBPushConfig
		wantInclude []string
		wantExclude []string
		wantErr     bool
	}{
		{
			name:        "config include used when no flags",
			duck:        config.DuckDBConfig{Projects: []string{"a", "b"}},
			wantInclude: []string{"a", "b"},
		},
		{
			name:        "flag include overrides config exclude",
			duck:        config.DuckDBConfig{ExcludeProjects: []string{"x"}},
			cfg:         DuckDBPushConfig{ProjectsFlag: "a,b"},
			wantInclude: []string{"a", "b"},
		},
		{
			name: "all-projects clears both",
			duck: config.DuckDBConfig{Projects: []string{"a"}},
			cfg:  DuckDBPushConfig{AllProjects: true},
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
			name: "config has both projects and exclude is an error",
			duck: config.DuckDBConfig{
				Projects:        []string{"a"},
				ExcludeProjects: []string{"x"},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inc, exc, err := resolveDuckDBPushProjects(tt.duck, tt.cfg)
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

func TestArchiveWriteBackendDuckDBPushPostsToDaemon(t *testing.T) {
	var gotAuth string
	absPath := filepath.Join(t.TempDir(), "agentsview.duckdb")
	ts := pushRuntimeServer(t, "/api/v1/push/duckdb", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		gotAuth = r.Header.Get("Authorization")
		var req daemonPushRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.True(t, req.Full)
		assert.Equal(t, []string{"a"}, req.Projects)
		assert.Equal(t, []string{"b"}, req.ExcludeProjects)
		require.NotNil(t, req.DuckDB)
		assert.Equal(t, absPath, req.DuckDB.Path)
		assert.Equal(t, "workstation", req.DuckDB.MachineName)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(duckdbsync.PushResult{
			SessionsPushed: 2,
			MessagesPushed: 3,
			Duration:       time.Second,
		}))
	})

	backend := daemonArchiveWriteBackend{
		appCfg: config.Config{AuthToken: "secret"},
		tr: transport{
			Mode: transportHTTP,
			URL:  ts.URL,
		},
	}
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
	assert.Equal(t, "Bearer secret", gotAuth)
	assert.Equal(t, 2, result.SessionsPushed)
	assert.Equal(t, 3, result.MessagesPushed)
}

func TestArchiveWriteBackendDuckDBPushAbsolutizesRelativeDaemonPath(t *testing.T) {
	var gotPath string
	ts := pushRuntimeServer(t, "/api/v1/push/duckdb", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		var req daemonPushRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.NotNil(t, req.DuckDB)
		gotPath = req.DuckDB.Path
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(duckdbsync.PushResult{}))
	})

	backend := daemonArchiveWriteBackend{
		tr: transport{
			Mode: transportHTTP,
			URL:  ts.URL,
		},
	}
	_, err := backend.DuckDBPush(
		context.Background(),
		config.DuckDBConfig{Path: "relative.duckdb"},
		DuckDBPushConfig{},
		nil,
		nil,
	)
	require.NoError(t, err)

	want, err := filepath.Abs("relative.duckdb")
	require.NoError(t, err)
	assert.Equal(t, want, gotPath)
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
