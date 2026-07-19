package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
)

func TestDuckDBLongRunningSignalsIncludeSIGTERM(t *testing.T) {
	signals := duckDBLongRunningSignals()
	assert.Contains(t, signals, os.Interrupt)
	assert.Contains(t, signals, syscall.SIGTERM)
}

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

// TestArchiveWriteBackendDuckDBPushPostsRemoteURLToDaemon verifies that a
// remote Quack URL is rejected client-side before any request reaches the
// daemon: push now writes the local mirror only.
func TestArchiveWriteBackendDuckDBPushPostsRemoteURLToDaemon(t *testing.T) {
	duckCfg := config.DuckDBConfig{
		URL:           "quack:127.0.0.1:9494",
		Token:         "quack-token",
		AllowInsecure: true,
	}
	ts := pushRuntimeServer(t, "/api/v1/push/duckdb", func(
		w http.ResponseWriter, r *http.Request,
	) {
		t.Fatal("daemon push route should not be called for a rejected remote target")
	})

	backend := newDaemonArchiveWriteBackendForTest(
		config.Config{AuthToken: "secret"}, ts.URL,
	)
	_, err := backend.DuckDBPush(
		context.Background(),
		duckCfg,
		DuckDBPushConfig{Full: true},
		[]string{"a"},
		[]string{"b"},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duckdb push writes the local mirror")
	assert.Contains(t, err.Error(), "quack serve")
}

func TestArchiveWriteBackendDuckDBPushWatchReResolvesDaemon(t *testing.T) {
	dataDir := t.TempDir()
	mirrorPath := filepath.Join(t.TempDir(), "mirror.duckdb")
	ctx, cancel := context.WithCancel(context.Background())
	var startupPushes int
	startup := pushRuntimeServer(t, "/api/v1/push/duckdb", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		startupPushes++
		var req daemonPushRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.NotNil(t, req.DuckDB)
		assert.Equal(t, mirrorPath, req.DuckDB.Path)
		writeTestJSON(t, w, duckdbsync.PushResult{SessionsPushed: 1})
	})
	var resolvedPushes int
	resolved := pushRuntimeServer(t, "/api/v1/push/duckdb", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		resolvedPushes++
		cancel()
		var req daemonPushRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.NotNil(t, req.DuckDB)
		assert.Equal(t, mirrorPath, req.DuckDB.Path)
		writeTestJSON(t, w, duckdbsync.PushResult{SessionsPushed: 1})
	})
	registerTestRuntime(t, dataDir, resolved.URL, false)

	backend := newDaemonArchiveWriteBackendForTest(
		config.Config{DataDir: dataDir}, startup.URL,
	)
	err := backend.DuckDBPushWatch(
		ctx,
		config.DuckDBConfig{
			Path: mirrorPath,
		},
		DuckDBPushConfig{},
		nil,
		nil,
		time.Millisecond,
		time.Millisecond,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, startupPushes)
	assert.GreaterOrEqual(t, resolvedPushes, 1)
	assert.NoFileExists(t, filepath.Join(dataDir, "sessions.db"))
}

// TestWriteDuckDBPushPlanDescribesLocalTarget verifies the printed plan
// always names the local mirror file: push writes the local mirror only, so
// there is no remote Quack endpoint branch (and no remote secret) to
// describe.
func TestWriteDuckDBPushPlanDescribesLocalTarget(t *testing.T) {
	var out bytes.Buffer
	duckCfg := config.DuckDBConfig{
		Path:        "/data/agentsview.duckdb",
		MachineName: "workstation",
	}

	writeDuckDBPushPlan(
		&out,
		duckCfg,
		DuckDBPushConfig{Full: true},
		[]string{"alpha", "beta"},
		nil,
		"url-abc123",
	)

	got := out.String()
	assert.Contains(t, got, "DuckDB push target: local file /data/agentsview.duckdb")
	assert.Contains(t, got, `machine "workstation"`)
	assert.Contains(t, got, "mode full")
	assert.Contains(t, got, "sync scope url-abc123")
	assert.Contains(t, got, "DuckDB push filters: include projects alpha, beta")
}

func TestWriteDuckDBPushDiagnosticsIncludesAgentBreakdown(t *testing.T) {
	var out bytes.Buffer

	writeDuckDBPushDiagnostics(&out, duckdbsync.PushResult{
		SessionsPushed: 3,
		MessagesPushed: 7,
		Diagnostics: duckdbsync.PushDiagnostics{
			Cutoff: "2026-07-01T12:00:00.000Z",
			LocalSessions: duckdbsync.PushSessionCounts{
				Total:   3,
				ByAgent: map[string]int{"codex": 1, "claude": 2},
			},
			CandidateSessions: duckdbsync.PushSessionCounts{
				Total:   3,
				ByAgent: map[string]int{"codex": 1, "claude": 2},
			},
			SkippedUnchangedSessions: duckdbsync.PushSessionCounts{
				Total: 0,
			},
			PushedSessions: duckdbsync.PushSessionCounts{
				Total:   3,
				ByAgent: map[string]int{"codex": 1, "claude": 2},
			},
			DeletedStaleSessions: 1,
		},
	})

	got := out.String()
	assert.Contains(t, got, "DuckDB push source: local 3 (claude=2, codex=1); candidates 3 (claude=2, codex=1); skipped unchanged 0; stale deleted 1")
	assert.Contains(t, got, "DuckDB push wrote: sessions 3 (claude=2, codex=1), messages 7")
}

func TestWriteDuckDBQuackServeStartupDoesNotPrintToken(t *testing.T) {
	var out bytes.Buffer
	const token = "plain-quack-secret-token"

	writeDuckDBQuackServeStartup(
		&out,
		duckDBQuackServeStartup{
			Path: "/tmp/agentsview.duckdb",
			Bind: "quack:127.0.0.1:9494",
			Info: quackServeInfo{ListenURI: "quack:127.0.0.1:9494"},
		},
	)

	got := out.String()
	assert.NotContains(t, got, token)
	assert.Contains(t, got, "Token:       configured")
}

// wantDuckDBDaemonPush is the expected shape of a DuckDB daemon push request.
type wantDuckDBDaemonPush struct {
	auth            string
	full            bool
	projects        []string
	excludeProjects []string
	path            string
	url             string
	token           string
	machineName     string
	allowInsecure   bool
	syncStateTarget string
}

// duckDBPushDaemonServer starts a daemon test server on the DuckDB push route
// that asserts the decoded request matches want and replies with result.
func duckDBPushDaemonServer(
	t *testing.T,
	want wantDuckDBDaemonPush,
	result duckdbsync.PushResult,
) *httptest.Server {
	t.Helper()
	return duckDBPushDaemonServerAt(t, "/api/v1/push/duckdb", want, result)
}

func duckDBPushDaemonServerAt(
	t *testing.T,
	path string,
	want wantDuckDBDaemonPush,
	result duckdbsync.PushResult,
) *httptest.Server {
	t.Helper()
	return pushRuntimeServer(t, path, func(
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
		assert.Equal(t, want.url, req.DuckDB.URL)
		assert.Equal(t, want.token, req.DuckDB.Token)
		assert.Equal(t, want.machineName, req.DuckDB.MachineName)
		assert.Equal(t, want.allowInsecure, req.DuckDB.AllowInsecure)
		assert.Equal(t, want.syncStateTarget, req.SyncStateTarget)
		writeTestJSON(t, w, result)
	})
}

func TestResolveQuackServeToken(t *testing.T) {
	tests := []struct {
		name       string
		flagToken  string
		configured string
		wantToken  string
		wantErr    string
	}{
		{
			name:       "flag token wins",
			flagToken:  "flag-token",
			configured: "config-token",
			wantToken:  "flag-token",
		},
		{
			name:       "configured token used",
			configured: "config-token",
			wantToken:  "config-token",
		},
		{
			name:    "requires explicit token",
			wantErr: "token is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := resolveQuackServeToken(
				tt.flagToken, tt.configured,
			)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantToken, token)
		})
	}
}
