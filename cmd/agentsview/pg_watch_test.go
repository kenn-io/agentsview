package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
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
		assert.Equal(t, "work", req.SyncStateTarget)
		assert.True(t, req.MigrateLegacySyncState)
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
		pgTargetSelection{
			PG: config.PGConfig{
				URL:           "postgres://user:pass@host/db",
				Schema:        "mirror",
				MachineName:   "laptop",
				AllowInsecure: true,
			},
			SyncStateTarget:        "work",
			MigrateLegacySyncState: true,
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
		pgTargetSelection{
			PG: config.PGConfig{
				URL: "postgres://user:pass@host/db",
			},
		},
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
	onPush     func()
	pushes     int
	closed     int
}

func (f *fakeTarget) EnsureSchema(context.Context) error { return f.ensureErr }
func (f *fakeTarget) Push(
	context.Context, bool, func(postgres.PushProgress),
) (postgres.PushResult, error) {
	f.pushes++
	if f.onPush != nil {
		f.onPush()
	}
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
		localSync:     func(context.Context) error { return nil },
		ensurePricing: func(context.Context) error { return nil },
		connect: func() (pgTarget, error) {
			tgt := targets[rec.connects]
			rec.connects++
			return tgt, nil
		},
	}
	return p, rec
}

func TestPGPusherEnsuresPricingAfterLocalSyncBeforeConnect(t *testing.T) {
	var events []string
	target := &fakeTarget{
		onPush: func() { events = append(events, "push") },
	}
	pusher := &pgPusher{
		localSync: func(context.Context) error {
			events = append(events, "local sync")
			return nil
		},
		ensurePricing: func(context.Context) error {
			events = append(events, "pricing ensure")
			return nil
		},
		connect: func() (pgTarget, error) {
			events = append(events, "connect")
			return target, nil
		},
	}

	require.NoError(t, pusher.push(
		context.Background(), reasonChange, false,
	))
	assert.Equal(t, []string{
		"local sync", "pricing ensure", "connect", "push",
	}, events)
}

func TestPGPusherPricingFailureWarnsAndContinues(t *testing.T) {
	wantErr := errors.New("catalog unavailable")
	target := &fakeTarget{}
	logs := captureLogOutput(t)
	pusher := &pgPusher{
		localSync:     func(context.Context) error { return nil },
		ensurePricing: func(context.Context) error { return wantErr },
		connect:       func() (pgTarget, error) { return target, nil },
	}

	require.NoError(t, pusher.push(
		context.Background(), reasonChange, false,
	))
	assert.Equal(t, 1, target.pushes)
	assert.Contains(t, logs.String(), "pricing refresh failed")
	assert.Contains(t, logs.String(), wantErr.Error())
}

func TestPGPusherCanceledPricingStopsBeforeConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	connectCalled := false
	pusher := &pgPusher{
		localSync: func(context.Context) error { return nil },
		ensurePricing: func(got context.Context) error {
			require.Equal(t, ctx, got)
			cancel()
			return got.Err()
		},
		connect: func() (pgTarget, error) {
			connectCalled = true
			return &fakeTarget{}, nil
		},
	}

	err := pusher.push(ctx, reasonChange, false)

	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, connectCalled)
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
	require.Error(t, p.push(context.Background(), reasonChange, false),
		"partial pushes must remain pending for retry")

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
	_, _, _, err := resolveWatchTargets(
		appCfg, PGPushConfig{}, "",
	)
	require.Error(t, err, "expected error when url not configured")
}

func TestResolveWatchTargets_ResolvesProjects(t *testing.T) {
	appCfg := config.Config{
		PG: config.PGConfig{
			URL:         "postgres://u:p@localhost:5432/db?sslmode=disable",
			MachineName: "box1",
		},
	}
	target, inc, _, err := resolveWatchTargets(
		appCfg, PGPushConfig{ProjectsFlag: "a,b"}, "",
	)
	require.NoError(t, err)
	assert.NotEmpty(t, target.PG.URL, "expected resolved URL")
	assert.Equal(t, []string{"a", "b"}, inc)
}

func TestResolveWatchTargets_IgnoresBrokenUnselectedTarget(t *testing.T) {
	restoreUnsetEnv(t, "BROKEN_WORK_TARGET")
	appCfg := config.Config{
		DefaultPG: "work",
		PGTargets: map[string]config.PGConfig{
			"work": {
				URL:         "${BROKEN_WORK_TARGET}",
				MachineName: "workbox",
			},
			"archive": {
				URL:         "postgres://archive",
				MachineName: "archivebox",
			},
		},
	}

	target, _, _, err := resolveWatchTargets(
		appCfg, PGPushConfig{}, "archive",
	)
	require.NoError(t, err)
	assert.Equal(t, "archive", target.Name)
	assert.Equal(t, "postgres://archive", target.PG.URL)
}

func TestResolvePGTargetSelections_DefaultAndAll(t *testing.T) {
	appCfg := config.Config{
		DefaultPG: "work",
		PGTargets: map[string]config.PGConfig{
			"work":    {URL: "postgres://work", MachineName: "workbox"},
			"archive": {URL: "postgres://archive", MachineName: "archivebox"},
		},
	}

	defaultTarget, err := resolvePGTargetSelections(
		appCfg, "", false,
	)
	require.NoError(t, err)
	require.Len(t, defaultTarget, 1)
	assert.Equal(t, "work", defaultTarget[0].Name)
	assert.True(t, defaultTarget[0].IsDefault)
	assert.Equal(t, "work", defaultTarget[0].SyncStateTarget)
	assert.True(t, defaultTarget[0].MigrateLegacySyncState)
	assert.Empty(t, defaultTarget[0].PG.URL)

	allTargets, err := resolvePGTargetSelections(
		appCfg, "", true,
	)
	require.NoError(t, err)
	require.Len(t, allTargets, 2)
	assert.Equal(t, "work", allTargets[0].Name)
	assert.Equal(t, "archive", allTargets[1].Name)
}

func TestResolvePGTargetConfig_IgnoresBrokenUnselectedTarget(t *testing.T) {
	restoreUnsetEnv(t, "BROKEN_WORK_TARGET")
	appCfg := config.Config{
		DefaultPG: "work",
		PGTargets: map[string]config.PGConfig{
			"work": {
				URL:         "${BROKEN_WORK_TARGET}",
				MachineName: "workbox",
			},
			"archive": {
				URL:         "postgres://archive",
				MachineName: "archivebox",
			},
		},
	}

	target, err := resolvePGTargetConfig(
		appCfg,
		pgTargetSelection{Name: "archive"},
	)
	require.NoError(t, err)
	assert.Equal(t, "postgres://archive", target.PG.URL)
}

func restoreUnsetEnv(t *testing.T, name string) {
	t.Helper()
	oldValue, hadValue := os.LookupEnv(name)
	require.NoError(t, os.Unsetenv(name))
	t.Cleanup(func() {
		if hadValue {
			require.NoError(t, os.Setenv(name, oldValue))
			return
		}
		require.NoError(t, os.Unsetenv(name))
	})
}

func TestResolvePGTargetSelections_RejectsLegacyNamedLookup(t *testing.T) {
	appCfg := config.Config{
		PG: config.PGConfig{
			URL:         "postgres://legacy",
			MachineName: "legacybox",
		},
	}

	_, err := resolvePGTargetSelections(
		appCfg, "archive", false,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "single legacy [pg] block")
}

func TestResolvePGTargetSelections_RejectsTargetWithAll(t *testing.T) {
	appCfg := config.Config{
		DefaultPG: "work",
		PGTargets: map[string]config.PGConfig{
			"work": {URL: "postgres://work"},
		},
	}

	_, err := resolvePGTargetSelections(
		appCfg, "work", true,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be combined with --all")
}

func TestNewPGPushCommandRejectsAllWatch(t *testing.T) {
	cmd := newPGPushCommand()
	cmd.SetArgs([]string{"--all", "--watch"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all cannot be combined with --watch")
}
