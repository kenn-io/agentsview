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
	"go.kenn.io/agentsview/internal/postgres"
)

func TestResolvePushProjects(t *testing.T) {
	tests := []projectResolutionCase[PGPushConfig]{
		{
			name:        "config include used when no flags",
			projects:    []string{"a", "b"},
			wantInclude: []string{"a", "b"},
		},
		{
			name:        "flag include overrides config exclude",
			exclude:     []string{"x"},
			cfg:         PGPushConfig{ProjectsFlag: "a,b"},
			wantInclude: []string{"a", "b"},
		},
		{
			name:     "all-projects clears both",
			projects: []string{"a"},
			cfg:      PGPushConfig{AllProjects: true},
		},
		{
			name:    "both flags is an error",
			cfg:     PGPushConfig{ProjectsFlag: "a", ExcludeProjects: "b"},
			wantErr: true,
		},
		{
			name:    "all-projects with include is an error",
			cfg:     PGPushConfig{AllProjects: true, ProjectsFlag: "a"},
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
			cfg:     PGPushConfig{AllProjects: true, ExcludeProjects: "x"},
			wantErr: true,
		},
	}
	runProjectResolutionCases(t, tests,
		func(projects, exclude []string, cfg PGPushConfig) ([]string, []string, error) {
			return resolvePushProjects(config.PGConfig{
				Projects:        projects,
				ExcludeProjects: exclude,
			}, cfg)
		},
	)
}

func TestArchiveWriteBackendPGPushPostsToDaemon(t *testing.T) {
	var gotAuth string
	ts := pushRuntimeServer(t, "/api/v1/push/pg", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		gotAuth = r.Header.Get("Authorization")
		var req daemonPushRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.True(t, req.Full)
		assert.Equal(t, []string{"a"}, req.Projects)
		assert.Equal(t, []string{"b"}, req.ExcludeProjects)
		require.NotNil(t, req.PG)
		assert.Equal(t, "postgres://user:pass@host/db", req.PG.URL)
		assert.Equal(t, "mirror", req.PG.Schema)
		assert.Equal(t, "laptop", req.PG.MachineName)
		assert.True(t, req.PG.AllowInsecure)
		writeTestJSON(t, w, postgres.PushResult{
			SessionsPushed: 2,
			MessagesPushed: 3,
			Duration:       time.Second,
		})
	})

	backend := newDaemonArchiveWriteBackendForTest(
		config.Config{AuthToken: "secret"}, ts.URL,
	)
	result, err := backend.PGPush(
		context.Background(),
		config.PGConfig{
			URL:           "postgres://user:pass@host/db",
			Schema:        "mirror",
			MachineName:   "laptop",
			AllowInsecure: true,
		},
		PGPushConfig{Full: true},
		[]string{"a"},
		[]string{"b"},
	)
	require.NoError(t, err)
	assert.Equal(t, "Bearer secret", gotAuth)
	assert.Equal(t, 2, result.SessionsPushed)
	assert.Equal(t, 3, result.MessagesPushed)
}

func TestResolveArchiveWriteBackendSkipsReadOnlyDaemon(t *testing.T) {
	dataDir := t.TempDir()
	called := false
	ts := pushRuntimeServer(t, "/api/v1/push/pg", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		called = true
		http.Error(w, "unexpected push", http.StatusInternalServerError)
	})
	registerTestRuntime(t, dataDir, ts.URL, true)

	backend, cleanup, err := resolveArchiveWriteBackend(
		context.Background(),
		config.Config{
			DataDir: dataDir,
			DBPath:  filepath.Join(dataDir, "sessions.db"),
		},
	)
	require.NoError(t, err)
	defer cleanup()
	assert.IsType(t, &localArchiveWriteBackend{}, backend)
	assert.False(t, called)
}

func TestArchiveWriteBackendPGPushWatchReResolvesDaemon(t *testing.T) {
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var startupPushes int
	startup := pushRuntimeServer(t, "/api/v1/push/pg", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		startupPushes++
		writeTestJSON(t, w, postgres.PushResult{SessionsPushed: 1})
	})
	var resolvedPushes int
	resolved := pushRuntimeServer(t, "/api/v1/push/pg", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		resolvedPushes++
		cancel()
		writeTestJSON(t, w, postgres.PushResult{SessionsPushed: 1})
	})
	registerTestRuntime(t, dataDir, resolved.URL, false)

	backend := newDaemonArchiveWriteBackendForTest(
		config.Config{DataDir: dataDir}, startup.URL,
	)
	err := backend.PGPushWatch(
		ctx,
		config.PGConfig{URL: "postgres://user:pass@host/db"},
		PGPushConfig{},
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

// fakeTarget is a test double for pgTarget.
type fakeTarget struct {
	ensureErr  error
	pushErr    error
	pushResult postgres.PushResult
	pushes     int
	closed     int
}

func (f *fakeTarget) EnsureSchema(context.Context) error { return f.ensureErr }
func (f *fakeTarget) Push(
	context.Context, bool, func(postgres.PushProgress),
) (postgres.PushResult, error) {
	f.pushes++
	return f.pushResult, f.pushErr
}
func (f *fakeTarget) Close() error { f.closed++; return nil }

// pusherRecorder tracks how many times a test pgPusher dialed a connection.
type pusherRecorder struct {
	connects int
}

// newTestPgPusher builds a pgPusher whose localSync is a no-op and whose
// connect hands out the supplied targets in order, recording each dial.
func newTestPgPusher(targets ...*fakeTarget) (*pgPusher, *pusherRecorder) {
	rec := &pusherRecorder{}
	p := &pgPusher{
		localSync: func(context.Context) error { return nil },
		connect: func() (pgTarget, error) {
			tgt := targets[rec.connects]
			rec.connects++
			return tgt, nil
		},
	}
	return p, rec
}

// requireReconnectAfterTargetError verifies that a push failure on first closes
// the target and the next push dials a fresh connection that succeeds.
func requireReconnectAfterTargetError(t *testing.T, first *fakeTarget) {
	t.Helper()
	p, rec := newTestPgPusher(first, &fakeTarget{})
	require.Error(t, p.push(context.Background(), reasonChange, false))
	require.Equal(t, 1, first.closed, "errored target should have been closed")
	require.NoError(t, p.push(context.Background(), reasonChange, false))
	require.Equal(t, 2, rec.connects, "should reconnect after error")
}

func TestPgPusher_ConnectsOnceAndReuses(t *testing.T) {
	target := &fakeTarget{}
	p, rec := newTestPgPusher(target)
	require.NoError(t, p.push(context.Background(), reasonChange, false))
	require.NoError(t, p.push(context.Background(), reasonChange, false))
	assert.Equal(t, 1, rec.connects, "connection should be reused")
	assert.Equal(t, 2, target.pushes)
}

func TestPgPusher_ReconnectsAfterPushError(t *testing.T) {
	requireReconnectAfterTargetError(t, &fakeTarget{pushErr: errors.New("conn reset")})
}

func TestPgPusher_ReconnectsAfterEnsureSchemaError(t *testing.T) {
	requireReconnectAfterTargetError(t, &fakeTarget{ensureErr: errors.New("schema down")})
}

func TestPgPusher_ConnectErrorSurfaced(t *testing.T) {
	p := &pgPusher{
		localSync: func(context.Context) error { return nil },
		connect: func() (pgTarget, error) {
			return nil, errors.New("dial timeout")
		},
	}
	require.Error(t, p.push(context.Background(), reasonChange, false))
}

func TestPgPusher_LocalSyncErrorSkipsConnect(t *testing.T) {
	connects := 0
	p := &pgPusher{
		localSync: func(context.Context) error { return errors.New("disk") },
		connect: func() (pgTarget, error) {
			connects++
			return &fakeTarget{}, nil
		},
	}
	require.Error(t, p.push(context.Background(), reasonChange, false))
	assert.Equal(t, 0, connects, "connect should not run when local sync fails")
}

func TestPgPusher_LogsPartialPushErrors(t *testing.T) {
	target := &fakeTarget{
		pushResult: postgres.PushResult{
			SessionsPushed: 3,
			MessagesPushed: 9,
			Errors:         2,
		},
	}
	logs := captureLogOutput(t)

	p, _ := newTestPgPusher(target)
	require.NoError(t, p.push(context.Background(), reasonChange, false))

	got := logs.String()
	assert.Contains(t, got, "pushed 3 sessions, 9 messages, 2 errors")
	assert.Contains(t, got, "2 session(s) failed to push; will retry")
	assert.Contains(t, got, "change")
}

func TestPgPusher_LogsSkippedConflicts(t *testing.T) {
	target := &fakeTarget{
		pushResult: postgres.PushResult{
			SessionsPushed:   3,
			MessagesPushed:   9,
			SkippedConflicts: 2,
		},
	}
	logs := captureLogOutput(t)

	p, _ := newTestPgPusher(target)
	require.NoError(t, p.push(context.Background(), reasonChange, false))

	got := logs.String()
	assert.Contains(t, got,
		"pushed 3 sessions, 9 messages, skipped 2 ownership conflict(s), 0 errors")
	assert.Contains(t, got,
		"2 session(s) skipped due to PostgreSQL ownership conflicts")
	assert.Contains(t, got, "change")
}

func TestResolveWatchTargets_ErrorsOnEmptyURL(t *testing.T) {
	appCfg := config.Config{} // no PG URL
	_, _, _, err := resolveWatchTargets(appCfg, PGPushConfig{})
	require.Error(t, err, "expected error when url not configured")
}

func TestResolveWatchTargets_ResolvesProjects(t *testing.T) {
	appCfg := config.Config{
		PG: config.PGConfig{
			URL:         "postgres://u:p@localhost:5432/db?sslmode=disable",
			MachineName: "box1",
		},
	}
	pg, inc, _, err := resolveWatchTargets(
		appCfg, PGPushConfig{ProjectsFlag: "a,b"},
	)
	require.NoError(t, err)
	assert.NotEmpty(t, pg.URL, "expected resolved URL")
	assert.Equal(t, []string{"a", "b"}, inc)
}
