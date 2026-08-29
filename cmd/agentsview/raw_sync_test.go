package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type recordingRawSyncRootRegistrar struct {
	roots []syncpkg.WatchRoot
}

type recordingRawSyncLoopWorker struct {
	audits int
	drains int
}

func (w *recordingRawSyncLoopWorker) AuditAll(context.Context) error {
	w.audits++
	return nil
}

func (w *recordingRawSyncLoopWorker) Drain(context.Context) error {
	w.drains++
	return nil
}

func (r *recordingRawSyncRootRegistrar) RegisterRoots(
	roots []syncpkg.WatchRoot,
	_ int,
) []syncpkg.RecursiveWatchResult {
	r.roots = append(r.roots, roots...)
	results := make([]syncpkg.RecursiveWatchResult, len(roots))
	for i, root := range roots {
		if root.Exists {
			results[i].Watched = 1
		}
	}
	return results
}

func TestValidateRawSyncWatchConfigRejectsUnsafeServerURLs(t *testing.T) {
	base := rawSyncWatchConfig{
		Server: "https://sync.example.test", DeviceID: "device-a",
		Debounce: time.Second, Interval: time.Minute, AuditLimit: 1,
	}
	for _, server := range []string{
		"http://sync.example.test",
		"ftp://sync.example.test",
		"https://user:private-value@sync.example.test",
		"https://sync.example.test?private=value",
		"https://sync.example.test/#private-value",
	} {
		cfg := base
		cfg.Server = server

		err := validateRawSyncWatchConfig(cfg, "credential-value")

		require.Error(t, err)
		assert.NotContains(t, err.Error(), "private-value")
		assert.NotContains(t, err.Error(), "credential-value")
	}
}

func TestValidateRawSyncWatchConfigAllowsExplicitLoopbackHTTP(t *testing.T) {
	for _, server := range []string{
		"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080",
	} {
		cfg := rawSyncWatchConfig{
			Server: server, DeviceID: "device-a", AllowInsecureHTTP: true,
			Debounce: time.Second, Interval: time.Minute, AuditLimit: 1,
		}

		require.NoError(t, validateRawSyncWatchConfig(cfg, "credential-value"))
	}
}

func TestValidateRawSyncWatchConfigRejectsNonLoopbackHTTPOverride(t *testing.T) {
	cfg := rawSyncWatchConfig{
		Server: "http://sync.example.test", DeviceID: "device-a",
		AllowInsecureHTTP: true,
		Debounce:          time.Second, Interval: time.Minute, AuditLimit: 1,
	}

	err := validateRawSyncWatchConfig(cfg, "credential-value")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), cfg.Server)
}

func TestRawSyncWatchCallbackRedactsOperationalErrors(t *testing.T) {
	const sensitivePath = "/private/session/path"
	callback := rawSyncWatchCallback(func(
		context.Context, syncpkg.WatchBatch,
	) error {
		return errors.New("open " + sensitivePath)
	})

	err := callback(t.Context(), syncpkg.WatchBatch{})

	require.EqualError(t, err, "raw-sync watcher work failed")
	assert.NotContains(t, err.Error(), sensitivePath)
}

func TestRawSyncWatchCallbackRetainsRetryScope(t *testing.T) {
	const sensitivePath = "/private/session/path"
	tests := []struct {
		name  string
		batch syncpkg.WatchBatch
		want  syncpkg.WatchBatch
	}{
		{
			name: "paths",
			batch: syncpkg.WatchBatch{
				Paths: []string{sensitivePath}, LostEvents: true,
			},
			want: syncpkg.WatchBatch{
				Paths: []string{sensitivePath}, LostEvents: true,
			},
		},
		{
			name:  "reconciliation roots",
			batch: syncpkg.WatchBatch{ReconcileRoots: []string{sensitivePath}},
			want:  syncpkg.WatchBatch{ReconcileRoots: []string{sensitivePath}},
		},
		{
			name:  "full sync",
			batch: syncpkg.WatchBatch{FullSync: true, LostEvents: true},
			want:  syncpkg.WatchBatch{FullSync: true, LostEvents: true},
		},
		{
			name: "file rename",
			batch: syncpkg.WatchBatch{Renames: []syncpkg.WatchRename{{
				Path: sensitivePath, ItemType: syncpkg.ItemIsFile,
			}}},
			want: syncpkg.WatchBatch{FullSync: true},
		},
		{
			name: "ambiguous rename",
			batch: syncpkg.WatchBatch{Renames: []syncpkg.WatchRename{{
				Path: sensitivePath, ItemType: syncpkg.ItemIsUnknown,
			}}, LostEvents: true},
			want: syncpkg.WatchBatch{FullSync: true, LostEvents: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callback := rawSyncWatchCallback(func(
				context.Context, syncpkg.WatchBatch,
			) error {
				return errors.New("open " + sensitivePath)
			})

			err := callback(t.Context(), tt.batch)

			require.EqualError(t, err, "raw-sync watcher work failed")
			assert.NotContains(t, err.Error(), sensitivePath)
			var retryErr syncpkg.WatchRetryError
			require.ErrorAs(t, err, &retryErr)
			assert.Equal(t, tt.want, retryErr.WatchRetryBatch())
		})
	}
}

func TestRunRawSyncLoopAuditsRetriesAndStopsOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		worker := &recordingRawSyncLoopWorker{}
		done := make(chan error, 1)
		go func() {
			done <- runRawSyncLoop(ctx, worker, time.Hour, time.Hour, nil)
		}()

		time.Sleep(time.Hour)
		synctest.Wait()
		assert.Equal(t, 1, worker.audits)
		assert.Equal(t, 1, worker.drains)
		cancel()
		synctest.Wait()
		require.NoError(t, <-done)
	})
}

func TestRawSyncStatusWithoutCheckpointDoesNotCreateState(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	output, err := executeCommand(newRootCommand(), "raw-sync", "status")

	require.NoError(t, err)
	assert.JSONEq(t, `{
		"device_id":"",
		"pending_generations":0,
		"pending_objects":0,
		"pending_object_bytes":0,
		"outbox":{"used_bytes":0,"reserved_bytes":0,"limit_bytes":0},
		"permanent_failures":0
	}`, output)
	_, err = os.Stat(filepath.Join(dataDir, "raw-sync"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestRawSyncMissingRootIsCountedAndRegisteredAfterCreation(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "later")
	roots := []syncpkg.WatchRoot{{Path: rootPath, Recursive: true, Exists: false}}
	registrar := &recordingRawSyncRootRegistrar{}

	pending, uncovered := registerRawSyncRoots(registrar, roots)

	assert.Equal(t, 1, uncovered)
	require.Len(t, pending, 1)
	require.Len(t, registrar.roots, 1)
	assert.False(t, registrar.roots[0].Exists)
	require.NoError(t, os.Mkdir(rootPath, 0o700))
	pending, err := refreshRawSyncRoots(registrar, pending)
	require.NoError(t, err)
	assert.Empty(t, pending)
	require.Len(t, registrar.roots, 2)
	assert.True(t, registrar.roots[1].Exists)
}

func TestRefreshRawSyncRootsKeepsStillMissingRoot(t *testing.T) {
	rootPath := filepath.Join(t.TempDir(), "still-missing")
	registrar := &recordingRawSyncRootRegistrar{}

	pending, err := refreshRawSyncRoots(registrar, []syncpkg.WatchRoot{{
		Path: rootPath, Recursive: true,
	}})

	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Empty(t, registrar.roots)
	_, statErr := os.Stat(rootPath)
	assert.True(t, errors.Is(statErr, os.ErrNotExist))
}

func TestRawSyncProvidersExcludeS3Roots(t *testing.T) {
	localRoot := t.TempDir()
	assert.Equal(t, []string{localRoot}, rawSyncFilesystemRoots([]string{
		"", localRoot, "s3://example-bucket/raw/claude",
	}))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentClaude: {localRoot, "s3://example-bucket/raw/claude"},
	}}

	providers, roots, err := rawSyncProvidersAndRoots(t.Context(), cfg)

	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Len(t, roots, 1)
	assert.Equal(t, localRoot, roots[0].Path)
}

func TestRawSyncProvidersNormalizeRelativeRootsForCapture(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	absoluteRoot := filepath.Join(base, "sessions")
	sessionPath := filepath.Join(absoluteRoot, "project", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o700))
	require.NoError(t, os.WriteFile(sessionPath, []byte("{}\n"), 0o600))
	relativeRoot := "sessions"
	require.False(t, filepath.IsAbs(relativeRoot))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{
		parser.AgentClaude: {relativeRoot},
	}}

	providers, roots, err := rawSyncProvidersAndRoots(t.Context(), cfg)
	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Len(t, roots, 1)
	assert.True(t, filepath.IsAbs(roots[0].Path))
	discovery, err := parser.DiscoverRawCaptureSources(t.Context(), providers[0])
	require.NoError(t, err)
	require.True(t, discovery.Complete)
	require.Len(t, discovery.Sources, 1)
	plan, supported, err := parser.ResolveRawCapturePlan(
		t.Context(), providers[0], discovery.Sources[0],
	)
	require.NoError(t, err)
	require.True(t, supported)
	assert.True(t, filepath.IsAbs(plan.ConfiguredRoot))
	watchedRoot, err := os.Stat(roots[0].Path)
	require.NoError(t, err)
	plannedRoot, err := os.Stat(plan.ConfiguredRoot)
	require.NoError(t, err)
	assert.True(t, os.SameFile(watchedRoot, plannedRoot),
		"capture plan must retain the normalized watched root")
}
