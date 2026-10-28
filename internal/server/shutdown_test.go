package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// TestShutdownClosesOnDemandEngine guards the lifecycle of the
// server-owned lazily-created sync engine: Shutdown must close it so
// pending debounced signal recomputes flush before the owner closes
// the DB. Injected engines are closed by their owner, not here.
func TestShutdownClosesOnDemandEngine(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	srv := New(config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		WriteTimeout: 30 * time.Second,
	}, database, nil)

	require.NotNil(t, srv.syncEngineForLocal(t.Context(), database),
		"on-demand engine should be created lazily")

	require.NoError(t, srv.Shutdown(t.Context()))

	srv.mu.RLock()
	defer srv.mu.RUnlock()
	assert.Nil(t, srv.onDemandEngine,
		"shutdown must close and release the on-demand engine")
}

func TestShutdownCancelsOnDemandArchiveSources(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	srv := New(config.Config{
		DataDir: t.TempDir(),
		ArchiveSources: []config.ArchiveSource{{
			Name: "saved", Repository: filepath.Join(t.TempDir(), "offline"), Snapshot: "pinned",
		}},
	}, database, nil)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	engine := srv.syncEngineForLocal(t.Context(), database)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := srv.finishLocalSync(ctx, engine, syncpkg.SyncStats{}, nil, false)
	require.ErrorIs(t, err, context.Canceled)
	manager := srv.onDemandArchiveSources
	require.NotNil(t, manager)
	_, err = manager.Drain(t.Context(), nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, context.Canceled, "the first request must not own later attachment work")

	require.NoError(t, srv.Shutdown(t.Context()))
	_, err = manager.Drain(t.Context(), nil)
	require.ErrorIs(t, err, context.Canceled, "shutdown must cancel attachment work before closing the engine")
}

func TestOnDemandEngineInitializationSurvivesRequestCancellation(t *testing.T) {
	for _, withBase := range []bool{false, true} {
		name := "default_lifecycle"
		if withBase {
			name = "configured_lifecycle"
		}
		t.Run(name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			path := filepath.Join(t.TempDir(), "skipped.jsonl")
			want := map[string]int64{path: 123}
			require.NoError(t, database.ReplaceSkippedFiles(t.Context(), want))
			srv := New(config.Config{}, database, nil)
			if withBase {
				srv.baseCtx = t.Context()
			}
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			engine := srv.syncEngineForLocal(ctx, database)
			assert.Equal(t, want, engine.SnapshotSkipCache())
			migrated, err := database.GetSyncState(t.Context(), "codex_exec_legacy_migration_v1")
			require.NoError(t, err)
			assert.NotEmpty(t, migrated)
			assert.Same(t, engine, srv.syncEngineForLocal(t.Context(), database))
		})
	}
}
