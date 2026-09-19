package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/storage"
)

func TestResolvePushProjects(t *testing.T) {
	tests := []projectResolutionCase[ReplicaPushConfig]{
		{
			name:        "config include used when no flags",
			projects:    []string{"a", "b"},
			wantInclude: []string{"a", "b"},
		},
		{
			name:        "flag include overrides config exclude",
			exclude:     []string{"x"},
			cfg:         ReplicaPushConfig{ProjectsFlag: "a,b"},
			wantInclude: []string{"a", "b"},
		},
		{
			name:     "all-projects clears both",
			projects: []string{"a"},
			cfg:      ReplicaPushConfig{AllProjects: true},
		},
		{
			name:    "both flags is an error",
			cfg:     ReplicaPushConfig{ProjectsFlag: "a", ExcludeProjects: "b"},
			wantErr: true,
		},
		{
			name:    "all-projects with include is an error",
			cfg:     ReplicaPushConfig{AllProjects: true, ProjectsFlag: "a"},
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
			cfg:     ReplicaPushConfig{AllProjects: true, ExcludeProjects: "x"},
			wantErr: true,
		},
	}
	runProjectResolutionCases(t, tests,
		func(projects, exclude []string, cfg ReplicaPushConfig) ([]string, []string, error) {
			return resolvePushProjects(storage.ConfiguredReplica{
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
		var req apiclient.DaemonPushRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
			return
		}
		assert.True(t, req.Full)
		assert.Equal(t, []string{"a"}, req.Projects)
		assert.Equal(t, []string{"b"}, req.ExcludeProjects)
		if !assert.NotNil(t, req.Replica) {
			return
		}
		assert.Equal(t, "postgres://user:pass@host/db", req.Replica.URL)
		assert.Equal(t, new("mirror"), req.Replica.Schema)
		assert.Equal(t, "laptop", req.Replica.MachineName)
		assert.Equal(t, new(true), req.Replica.AllowInsecure)
		assert.Equal(t, new("work"), req.SyncStateTarget)
		assert.Equal(t, new(true), req.MigrateLegacySyncState)
		writeTestJSON(t, w, storage.PushResult{
			SessionsPushed: 2,
			MessagesPushed: 3,
			Duration:       time.Second,
		})
	})

	backend := newDaemonArchiveWriteBackendForTest(
		config.Config{AuthToken: "secret"}, ts.URL,
	)
	result, err := backend.ReplicaPush(
		t.Context(), pgReplica{},
		storage.ConfiguredReplica{
			Name: "work", IsDefault: true,
			Target: storage.ReplicaTarget{
				URL:           "postgres://user:pass@host/db",
				Schema:        "mirror",
				MachineName:   "laptop",
				AllowInsecure: true,
			},
		},
		ReplicaPushConfig{Full: true},
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
		t.Context(),
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
	ctx, cancel := context.WithCancel(t.Context())
	var startupPushes int
	startup := pushRuntimeServer(t, "/api/v1/push/pg", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		startupPushes++
		writeTestJSON(t, w, storage.PushResult{SessionsPushed: 1})
	})
	var resolvedPushes int
	resolved := pushRuntimeServer(t, "/api/v1/push/pg", func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		resolvedPushes++
		cancel()
		writeTestJSON(t, w, storage.PushResult{SessionsPushed: 1})
	})
	registerTestRuntime(t, dataDir, resolved.URL, false)

	backend := newDaemonArchiveWriteBackendForTest(
		config.Config{DataDir: dataDir}, startup.URL,
	)
	err := backend.ReplicaPushWatch(
		ctx, pgReplica{},
		storage.ConfiguredReplica{Target: storage.ReplicaTarget{
			URL: "postgres://user:pass@host/db",
		}},
		ReplicaPushConfig{},
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

// fakeTarget is a test double for storage.Pusher.
type fakeTarget struct {
	ensureErr  error
	pushErr    error
	pushResult storage.PushResult
	onPush     func()
	pushes     int
	closed     int
	pushOpts   []storage.PushOptions // options of each push, in order
}

func (f *fakeTarget) EnsureSchema(context.Context) error { return f.ensureErr }
func (f *fakeTarget) PushWithOptions(
	_ context.Context, opts storage.PushOptions,
	_ func(storage.PushProgress),
) (storage.PushResult, error) {
	f.pushes++
	f.pushOpts = append(f.pushOpts, opts)
	if f.onPush != nil {
		f.onPush()
	}
	return f.pushResult, f.pushErr
}
func (f *fakeTarget) Close() error { f.closed++; return nil }

// pusherRecorder tracks how many times a test replicaPusher dialed a connection.
type pusherRecorder struct {
	connects int
}

// newTestReplicaPusher builds a replicaPusher whose localSync is a no-op and whose
// connect hands out the supplied targets in order, recording each dial.
func newTestReplicaPusher(targets ...*fakeTarget) (*replicaPusher, *pusherRecorder) {
	rec := &pusherRecorder{}
	p := &replicaPusher{
		label:         "pg watch",
		displayName:   "PostgreSQL",
		localSync:     func(context.Context) error { return nil },
		ensurePricing: func(context.Context) error { return nil },
		connect: func(context.Context) (storage.Pusher, error) {
			tgt := targets[rec.connects]
			rec.connects++
			return tgt, nil
		},
	}
	return p, rec
}

// TestPGPusherScopesChangeVectorPushes pins the watch scoping policy: only
// change-triggered pushes after a clean generation-wide vector
// reconciliation scope their vector phase; startup and the interval floor
// stay generation-wide, and a deferring vector phase forces the next push
// back to generation-wide before scoping resumes.
func TestPGPusherScopesChangeVectorPushes(t *testing.T) {
	target := &fakeTarget{pushResult: storage.PushResult{
		Vectors: storage.VectorPushResult{GenerationID: 1},
	}}
	pusher, _ := newTestReplicaPusher(target)
	pusher.vectorReconcileNeeded = true
	ctx := t.Context()

	require.NoError(t, pusher.push(ctx, reasonStartup, false))
	require.NoError(t, pusher.push(ctx, reasonChange, false))
	require.NoError(t, pusher.push(ctx, reasonInterval, false))
	target.pushResult = storage.PushResult{
		Vectors: storage.VectorPushResult{SessionsDeferred: 1},
	}
	require.NoError(t, pusher.push(ctx, reasonChange, false))
	target.pushResult = storage.PushResult{
		Vectors: storage.VectorPushResult{GenerationID: 1},
	}
	require.NoError(t, pusher.push(ctx, reasonChange, false))
	require.NoError(t, pusher.push(ctx, reasonChange, false))

	scoped := make([]bool, 0, len(target.pushOpts))
	for _, o := range target.pushOpts {
		scoped = append(scoped, o.ScopeVectorsToChangedSessions)
	}
	assert.Equal(t,
		[]bool{false, true, false, true, false, true}, scoped,
		"startup full-read, scoped change, full interval, scoped change that defers, reconciling change, scoped change")
}

// TestPGPusherZeroGenerationKeepsReconcile pins that a generation-wide
// phase reporting no generation id — a daemon predating the field — never
// clears the reconcile bit. Clearing on it would let the next change push
// scope with a zero memo, which the vector phase cannot promote if the
// generation was recreated meanwhile. Scoping resumes with the first
// response that carries an id.
func TestPGPusherZeroGenerationKeepsReconcile(t *testing.T) {
	target := &fakeTarget{}
	pusher, _ := newTestReplicaPusher(target)
	pusher.vectorReconcileNeeded = true
	ctx := t.Context()

	require.NoError(t, pusher.push(ctx, reasonStartup, false))
	require.NoError(t, pusher.push(ctx, reasonChange, false))
	target.pushResult = storage.PushResult{
		Vectors: storage.VectorPushResult{GenerationID: 3},
	}
	require.NoError(t, pusher.push(ctx, reasonInterval, false))
	require.NoError(t, pusher.push(ctx, reasonChange, false))

	require.Len(t, target.pushOpts, 4)
	assert.False(t, target.pushOpts[1].ScopeVectorsToChangedSessions,
		"a zero-id reconciliation must not enable scoping")
	assert.Zero(t, target.pushOpts[1].LastReconciledVectorGeneration,
		"no generation id was ever reported, so none is carried")
	assert.True(t, target.pushOpts[3].ScopeVectorsToChangedSessions,
		"scoping resumes once a response reports the reconciled id")
	assert.Equal(t, int64(3), target.pushOpts[3].LastReconciledVectorGeneration,
		"the scoped push carries the first reported generation id")
}

// TestPGPusherPushErrorForcesReconcile pins that any push error sends the
// next push back to a generation-wide vector reconciliation.
func TestPGPusherPushErrorForcesReconcile(t *testing.T) {
	failing := &fakeTarget{pushErr: errors.New("boom")}
	recovered := &fakeTarget{}
	pusher, _ := newTestReplicaPusher(failing, recovered)
	pusher.vectorReconcileNeeded = false
	ctx := t.Context()

	require.Error(t, pusher.push(ctx, reasonChange, false))
	require.NoError(t, pusher.push(ctx, reasonChange, false))

	require.Len(t, failing.pushOpts, 1)
	assert.True(t, failing.pushOpts[0].ScopeVectorsToChangedSessions)
	require.Len(t, recovered.pushOpts, 1)
	assert.False(t, recovered.pushOpts[0].ScopeVectorsToChangedSessions)
}

// TestPGPusherThreadsGenerationID pins the generation-id memo that keeps a
// scoped push from exposing an incomplete generation: the watch process
// threads the last generation-wide id into every push and advances it
// whenever a push reconciles a different generation, so a later scoped push
// always carries the current id for the vector phase to compare against.
func TestPGPusherThreadsGenerationID(t *testing.T) {
	target := &fakeTarget{}
	pusher, _ := newTestReplicaPusher(target)
	pusher.vectorReconcileNeeded = true
	ctx := t.Context()

	// Startup reconciles generation id 1 generation-wide.
	target.pushResult = storage.PushResult{
		Vectors: storage.VectorPushResult{GenerationID: 1},
	}
	require.NoError(t, pusher.push(ctx, reasonStartup, false))

	// A scoped change push carries id 1 and leaves the memo unchanged.
	require.NoError(t, pusher.push(ctx, reasonChange, false))

	// The active generation is recreated as id 2. The phase promotes itself
	// and reconciles it generation-wide, reported by the differing id, so the
	// memo advances to 2 while the reconcile bit stays clear.
	target.pushResult = storage.PushResult{
		Vectors: storage.VectorPushResult{GenerationID: 2},
	}
	require.NoError(t, pusher.push(ctx, reasonChange, false))

	// The next scoped change push now carries id 2.
	require.NoError(t, pusher.push(ctx, reasonChange, false))

	require.Len(t, target.pushOpts, 4)
	assert.Zero(t, target.pushOpts[0].LastReconciledVectorGeneration,
		"startup carries no prior generation id")
	assert.False(t, target.pushOpts[0].ScopeVectorsToChangedSessions)
	assert.Equal(t, int64(1), target.pushOpts[1].LastReconciledVectorGeneration,
		"the scoped change push carries the startup generation id")
	assert.True(t, target.pushOpts[1].ScopeVectorsToChangedSessions)
	assert.Equal(t, int64(1), target.pushOpts[2].LastReconciledVectorGeneration,
		"the switch push still carries id 1 so the phase can detect the change")
	assert.True(t, target.pushOpts[2].ScopeVectorsToChangedSessions)
	assert.Equal(t, int64(2), target.pushOpts[3].LastReconciledVectorGeneration,
		"once id 2 is reconciled the next scoped push carries it")
	assert.True(t, target.pushOpts[3].ScopeVectorsToChangedSessions)
}

func TestPGPusherEnsuresPricingAfterLocalSyncBeforeConnect(t *testing.T) {
	var events []string
	target := &fakeTarget{
		onPush: func() { events = append(events, "push") },
	}
	pusher := &replicaPusher{
		label: "pg watch", displayName: "PostgreSQL",
		localSync: func(context.Context) error {
			events = append(events, "local sync")
			return nil
		},
		ensurePricing: func(context.Context) error {
			events = append(events, "pricing ensure")
			return nil
		},
		connect: func(context.Context) (storage.Pusher, error) {
			events = append(events, "connect")
			return target, nil
		},
	}

	require.NoError(t, pusher.push(
		t.Context(), reasonChange, false,
	))
	assert.Equal(t, []string{
		"local sync", "pricing ensure", "connect", "push",
	}, events)
}

func TestPGPusherPricingFailureWarnsAndContinues(t *testing.T) {
	wantErr := errors.New("catalog unavailable")
	target := &fakeTarget{}
	logs := captureLogOutput(t)
	pusher := &replicaPusher{
		label: "pg watch", displayName: "PostgreSQL",
		localSync:     func(context.Context) error { return nil },
		ensurePricing: func(context.Context) error { return wantErr },
		connect:       func(context.Context) (storage.Pusher, error) { return target, nil },
	}

	require.NoError(t, pusher.push(
		t.Context(), reasonChange, false,
	))
	assert.Equal(t, 1, target.pushes)
	assert.Contains(t, logs.String(), "pricing refresh failed")
	assert.Contains(t, logs.String(), wantErr.Error())
}

func TestPGPusherCanceledPricingStopsBeforeConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	connectCalled := false
	pusher := &replicaPusher{
		label: "pg watch", displayName: "PostgreSQL",
		localSync: func(context.Context) error { return nil },
		ensurePricing: func(got context.Context) error {
			require.Equal(t, ctx, got)
			cancel()
			return got.Err()
		},
		connect: func(context.Context) (storage.Pusher, error) {
			connectCalled = true
			return &fakeTarget{}, nil
		},
	}

	err := pusher.push(ctx, reasonChange, false)

	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, connectCalled)
}

// requireReconnectAfterTargetError verifies that a push failure on first closes
// the target and the next push dials a fresh connection that succeeds.
func requireReconnectAfterTargetError(t *testing.T, first *fakeTarget) {
	t.Helper()

	p, rec := newTestReplicaPusher(first, &fakeTarget{})
	require.Error(t, p.push(t.Context(), reasonChange, false))
	require.Equal(t, 1, first.closed, "errored target should have been closed")
	require.NoError(t, p.push(t.Context(), reasonChange, false))
	require.Equal(t, 2, rec.connects, "should reconnect after error")
}

func TestPgPusher_ConnectsOnceAndReuses(t *testing.T) {
	target := &fakeTarget{}
	p, rec := newTestReplicaPusher(target)
	require.NoError(t, p.push(t.Context(), reasonChange, false))
	require.NoError(t, p.push(t.Context(), reasonChange, false))
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
	p := &replicaPusher{
		label: "pg watch", displayName: "PostgreSQL",
		localSync: func(context.Context) error { return nil },
		connect: func(context.Context) (storage.Pusher, error) {
			return nil, errors.New("dial timeout")
		},
	}
	require.Error(t, p.push(t.Context(), reasonChange, false))
}

func TestPgPusher_LocalSyncErrorSkipsConnect(t *testing.T) {
	connects := 0
	p := &replicaPusher{
		label: "pg watch", displayName: "PostgreSQL",
		localSync: func(context.Context) error { return errors.New("disk") },
		connect: func(context.Context) (storage.Pusher, error) {
			connects++
			return &fakeTarget{}, nil
		},
	}
	require.Error(t, p.push(t.Context(), reasonChange, false))
	assert.Equal(t, 0, connects, "connect should not run when local sync fails")
}

func TestPgPusher_LogsPartialPushErrors(t *testing.T) {
	target := &fakeTarget{
		pushResult: storage.PushResult{
			SessionsPushed: 3,
			MessagesPushed: 9,
			Errors:         2,
		},
	}
	logs := captureLogOutput(t)

	p, _ := newTestReplicaPusher(target)
	require.Error(t, p.push(t.Context(), reasonChange, false),
		"partial pushes must remain pending for retry")

	got := logs.String()
	assert.Contains(t, got, "pushed 3 sessions, 9 messages, 2 errors")
	assert.Contains(t, got, "2 session(s) failed to push; will retry")
	assert.Contains(t, got, "change")
}

func TestPgPusher_LogsSkippedConflicts(t *testing.T) {
	target := &fakeTarget{
		pushResult: storage.PushResult{
			SessionsPushed:   3,
			MessagesPushed:   9,
			SkippedConflicts: 2,
		},
	}
	logs := captureLogOutput(t)

	p, _ := newTestReplicaPusher(target)
	require.NoError(t, p.push(t.Context(), reasonChange, false))

	got := logs.String()
	assert.Contains(t, got,
		"pushed 3 sessions, 9 messages, skipped 2 ownership conflict(s), 0 errors")
	assert.Contains(t, got,
		"2 session(s) skipped due to PostgreSQL ownership conflicts")
	assert.Contains(t, got, "change")
}

func TestResolveWatchTargets_ErrorsOnEmptyURL(t *testing.T) {
	appCfg := config.Config{} // no PG URL
	_, _, _, err := resolveWatchTarget(
		pgReplica{}, appCfg, ReplicaPushConfig{}, "",
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
	target, inc, _, err := resolveWatchTarget(
		pgReplica{}, appCfg, ReplicaPushConfig{ProjectsFlag: "a,b"}, "",
	)
	require.NoError(t, err)
	assert.NotEmpty(t, target.Target.URL, "expected resolved URL")
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

	target, _, _, err := resolveWatchTarget(
		pgReplica{}, appCfg, ReplicaPushConfig{}, "archive",
	)
	require.NoError(t, err)
	assert.Equal(t, "archive", target.Name)
	assert.Equal(t, "postgres://archive", target.Target.URL)
}

func TestResolvePGTargetSelections_DefaultAndAll(t *testing.T) {
	appCfg := config.Config{
		DefaultPG: "work",
		PGTargets: map[string]config.PGConfig{
			"work":    {URL: "postgres://work", MachineName: "workbox"},
			"archive": {URL: "postgres://archive", MachineName: "archivebox"},
		},
	}

	defaultTarget, err := storage.SelectTargets(
		pgReplica{}, appCfg, "", false,
	)
	require.NoError(t, err)
	require.Len(t, defaultTarget, 1)
	assert.Equal(t, "work", defaultTarget[0].Name)
	assert.True(t, defaultTarget[0].IsDefault)
	assert.Equal(t, "work", defaultTarget[0].SyncStateTarget())
	assert.True(t, defaultTarget[0].MigrateLegacySyncState())

	allTargets, err := storage.SelectTargets(
		pgReplica{}, appCfg, "", true,
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

	target, err := pgReplica{}.ResolveTarget(
		appCfg, storage.ReplicaTargetRef{Name: "archive"},
	)
	require.NoError(t, err)
	assert.Equal(t, "postgres://archive", target.Target.URL)
}

func restoreUnsetEnv(t *testing.T, name string) {
	t.Helper()
	oldValue, hadValue := os.LookupEnv(name)
	if hadValue {
		t.Setenv(name, oldValue)
	} else {
		t.Setenv(name, "")
		t.Cleanup(func() { _ = os.Unsetenv(name) })
	}
	require.NoError(t, os.Unsetenv(name))
}

func TestResolvePGTargetSelections_RejectsLegacyNamedLookup(t *testing.T) {
	appCfg := config.Config{
		PG: config.PGConfig{
			URL:         "postgres://legacy",
			MachineName: "legacybox",
		},
	}

	_, err := storage.SelectTargets(
		pgReplica{}, appCfg, "archive", false,
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

	_, err := storage.SelectTargets(
		pgReplica{}, appCfg, "work", true,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be combined with --all")
}

func TestNewPGPushCommandRejectsAllWatch(t *testing.T) {
	cmd := newReplicaPushCommand(pgReplica{})
	cmd.SetArgs([]string{"--all", "--watch"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all cannot be combined with --watch")
}
