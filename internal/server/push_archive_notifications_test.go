package server

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestDaemonPushCopiesCommittedArchiveWithoutScanningSources(t *testing.T) {
	f := newSyncRouteFixture(t)
	path := f.writeClaudeSession(t, "proj/changed.jsonl", "committed message")
	engine := f.srv.syncEngineForLocal(t.Context(), f.db)
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	before, err := f.db.GetSessionFull(t.Context(), "changed")
	require.NoError(t, err)
	require.NotNil(t, before)
	f.writeClaudeSession(t, "proj/changed.jsonl", "not ingested yet")
	f.writeClaudeSession(t, "proj/unrelated.jsonl", "must not be discovered")

	var published *db.Session
	srv := New(f.srv.cfg, f.db, engine, WithReplicas(stubReplica{
		newPusher: func() (storage.Pusher, error) {
			return archiveSnapshotPusher{read: func(ctx context.Context) error {
				var err error
				published, err = f.db.GetSessionFull(ctx, "changed")
				return err
			}}, nil
		},
	}))
	response := serveJSON(t, srv.Handler(), http.MethodPost, "/api/v1/push/pg", map[string]any{
		"full":           false,
		"no_source_sync": true,
		"replica": map[string]any{
			"url":          "postgres://127.0.0.1:5432/test?sslmode=disable",
			"machine_name": "test",
		},
	})

	require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())
	assert.Equal(t, before, published)
	after, err := f.db.GetSessionFull(t.Context(), "changed")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	unrelated, err := f.db.GetSessionFull(t.Context(), "unrelated")
	require.NoError(t, err)
	assert.Nil(t, unrelated)
	assert.FileExists(t, filepath.Join(f.claudeDir, "proj", "unrelated.jsonl"))
}

func TestDaemonArchiveCopyRejectsMirrorWatchScope(t *testing.T) {
	for _, field := range []string{"watch_batch", "watch_recovery"} {
		t.Run(field, func(t *testing.T) {
			f := newSyncRouteFixture(t)
			response := serveJSON(t, f.handler, http.MethodPost, "/api/v1/push/duckdb", map[string]any{
				"full":           false,
				"no_source_sync": true,
				field:            map[string]any{},
			}, withAccept("text/event-stream"))
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.JSONEq(t, `{"error":"no_source_sync cannot be combined with a watch scope"}`, response.Body.String())
		})
	}
}

func TestDaemonArchivePushDoesNotBypassRequiredRebuild(t *testing.T) {
	for _, tt := range []struct {
		name string
		full bool
	}{
		{name: "stale archive"},
		{name: "explicit full push", full: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := []syncRouteFixtureOption{withLocalResyncRunner(func(
				context.Context, func(syncpkg.Progress),
			) (syncpkg.SyncStats, error) {
				return syncpkg.SyncStats{}, errors.New("rebuild temporarily unavailable")
			})}
			if !tt.full {
				opts = append(opts, withStaleDB())
			}
			f := newSyncRouteFixture(t, opts...)
			response := serveJSON(t, f.handler, http.MethodPost, "/api/v1/push/pg", map[string]any{
				"full": tt.full, "no_source_sync": true,
				"replica": map[string]any{
					"url":          "postgres://127.0.0.1:5432/test?sslmode=disable",
					"machine_name": "test",
				},
			})

			require.Equal(t, http.StatusInternalServerError, response.Code, "body: %s", response.Body.String())
			assert.Contains(t, response.Body.String(), "rebuild temporarily unavailable")
			assert.Contains(t, response.Body.String(), `"push_started":false`)
		})
	}
}

type archiveSnapshotPusher struct {
	read func(context.Context) error
}

func (archiveSnapshotPusher) EnsureSchema(context.Context) error { return nil }

func (p archiveSnapshotPusher) PushWithOptions(
	ctx context.Context, _ storage.PushOptions, _ func(storage.PushProgress),
) (storage.PushResult, error) {
	if err := p.read(ctx); err != nil {
		return storage.PushResult{}, err
	}
	return storage.PushResult{SessionsPushed: 1}, nil
}

func (archiveSnapshotPusher) Close() error { return nil }
