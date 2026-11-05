package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/archivepush"
	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const attachedPushSession = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa~saved"

func TestArchiveSourcesDrainAfterWatcherStartupFailure(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	cfg.ArchiveSources = []config.ArchiveSource{archiveSourceCommandFixture(t)}
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	engine := syncpkg.NewEngine(t.Context(), database, localSyncEngineConfig(cfg))
	t.Cleanup(engine.Close)
	watcher, err := syncpkg.NewWatcherWithCallback(0, 0,
		func(context.Context, syncpkg.WatchBatch) error { return nil }, nil, syncpkg.WatcherOptions{})
	require.NoError(t, err)
	t.Cleanup(watcher.Stop)
	// A backend startup failure closes the watcher before returning its error.
	flush := startWatcherCollection(func() error {
		watcher.Stop()
		return fmt.Errorf("start watcher backend: %w", os.ErrPermission)
	}, watcher.FlushPending)
	manager, err := archivesource.NewManager(t.Context(), cfg, database, engine, flush)
	require.NoError(t, err)
	t.Cleanup(manager.Close)

	stats, err := manager.Drain(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Synced)
	messages, err := database.GetAllMessages(t.Context(), attachedPushSession)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "archived message", messages[0].Content)
}

// Observe the archive at the destination boundary, before a push can finish.
type archiveSourcePushProbe struct {
	database *db.DB
	seen     chan []db.Message
	inspect  func(context.Context, *db.DB)
	full     bool
}

func (*archiveSourcePushProbe) EnsureSchema(context.Context) error { return nil }
func (*archiveSourcePushProbe) Close() error                       { return nil }
func (p *archiveSourcePushProbe) PushWithOptions(ctx context.Context, opts storage.PushOptions, _ func(storage.PushProgress)) (storage.PushResult, error) {
	p.full = opts.Full
	if p.inspect != nil {
		p.inspect(ctx, p.database)
	}
	messages, err := p.database.GetAllMessages(ctx, attachedPushSession)
	if err == nil {
		p.seen <- messages
	}
	return storage.PushResult{}, err
}

type archiveSourcePushReplica struct {
	storage.Replica
	probe *archiveSourcePushProbe
}

func (r archiveSourcePushReplica) NewPusher(_ context.Context, _ storage.ReplicaTarget, database *db.DB, _ storage.PusherOptions) (storage.Pusher, error) {
	r.probe.database = database
	return r.probe, nil
}

type archiveSourcePushMirror struct {
	duckdb.Mirror
	probe *archiveSourcePushProbe
}

func (m archiveSourcePushMirror) Push(ctx context.Context, _ config.DuckDBConfig, database *db.DB, _ storage.MirrorPushOptions, full bool, _ func(storage.MirrorPushProgress)) (storage.MirrorPushResult, error) {
	m.probe.database = database
	_, err := m.probe.PushWithOptions(ctx, storage.PushOptions{Full: full}, nil)
	return storage.MirrorPushResult{}, err
}

func TestArchiveSourceDaemonPushDrainsBeforeCopy(t *testing.T) {
	source := archiveSourceCommandFixture(t)
	for _, name := range []string{"pg", "clickhouse", "duckdb"} {
		for _, mode := range []string{"on-demand", "shared", "full-runner"} {
			for _, offline := range []bool{false, true} {
				t.Run(name+"/"+mode+"/offline="+strconv.FormatBool(offline), func(t *testing.T) {
					cfg := testConfigWithClaudeFixture(t)
					cfg.Host = "127.0.0.1"
					cfg.NoSync = mode == "on-demand"
					cfg.ArchiveSources = []config.ArchiveSource{source}
					database := dbtest.OpenTestDBAt(t, cfg.DBPath)
					if offline {
						_, _, err := runLocalSyncResult(t.Context(), cfg, database, false)
						require.NoError(t, err)
						require.NoError(t, os.Rename(source.Repository, source.Repository+"-offline"))
						t.Cleanup(func() { require.NoError(t, os.Rename(source.Repository+"-offline", source.Repository)) })
					}
					// This session has not been ingested when the push starts.
					dbtest.WriteTestFile(t, filepath.Join(cfg.AgentDirs[parser.AgentClaude][0], "project", "push-local.jsonl"),
						[]byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "new local message", "push-local").String()))

					probe := &archiveSourcePushProbe{seen: make(chan []db.Message, 2), inspect: func(ctx context.Context, database *db.DB) {
						messages, err := database.GetAllMessages(ctx, "push-local")
						require.NoError(t, err)
						if assert.Len(t, messages, 1) {
							assert.Equal(t, "new local message", messages[0].Content)
						}
					}}
					opts := []server.Option{
						server.WithBaseContext(t.Context()),
						server.WithReplicas(
							archiveSourcePushReplica{Replica: postgres.Backend{}, probe: probe},
							archiveSourcePushReplica{Replica: clickhouse.Backend{}, probe: probe},
						),
						server.WithMirror(archiveSourcePushMirror{probe: probe}),
					}
					var engine *syncpkg.Engine
					if !cfg.NoSync {
						engine = syncpkg.NewEngine(t.Context(), database, localSyncEngineConfig(cfg))
						t.Cleanup(engine.Close)
						manager, err := archivesource.NewManager(t.Context(), cfg, database, engine, nil)
						require.NoError(t, err)
						t.Cleanup(manager.Close)
						opts = append(opts, server.WithArchiveSources(manager))
						if mode == "full-runner" {
							opts = append(opts, server.WithLocalResyncRunner(server.LocalResyncRunner(withAttachedSources(manager,
								func(ctx context.Context, progress func(syncpkg.Progress)) (syncpkg.SyncStats, error) {
									return engine.SyncThenRun(ctx, true, progress, func(bool) error { return nil })
								}))))
						}
					}
					srv := server.New(cfg, database, engine, opts...)
					t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
					scheme := "postgres"
					if name == "clickhouse" {
						scheme = "clickhouse"
					}
					req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/push/"+name,
						strings.NewReader(fmt.Sprintf(`{"full":%t,"replica":{"url":"%s://localhost/test","machine_name":"receiver"}}`, mode == "full-runner", scheme)))
					req.Host = "127.0.0.1:0"
					req.RemoteAddr = "127.0.0.1:1234"
					req.Header.Set("Origin", "http://127.0.0.1:0")
					req.Header.Set("Content-Type", "application/json")
					response := httptest.NewRecorder()
					srv.Handler().ServeHTTP(response, req)
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					messages := receiveArchiveTest(t, probe.seen)
					require.Len(t, messages, 1, "the first push must include pending attached history")
					assert.Equal(t, "archived message", messages[0].Content)
					assert.Equal(t, mode == "full-runner", probe.full, "a rebuilt archive must still be pushed in full after an attachment warning")
				})
			}
		}
	}
}

func TestArchiveSourceDirectPushWithOfflineAttachment(t *testing.T) {
	source := archiveSourceCommandFixture(t)
	for _, name := range []string{"pg", "clickhouse", "duckdb"} {
		for _, mode := range []struct {
			name       string
			full       bool
			stale      bool
			localError bool
			wantFull   bool
		}{
			{name: "incremental"},
			{name: "full", full: true, wantFull: true},
			{name: "automatic-rebuild", stale: true, wantFull: true},
			{name: "local-write-failure", localError: true},
		} {
			t.Run(name+"/"+mode.name, func(t *testing.T) {
				cfg := testConfigWithClaudeFixture(t)
				cfg.ArchiveSources = []config.ArchiveSource{source}
				database := dbtest.OpenTestDBAt(t, cfg.DBPath)
				_, _, err := runLocalSyncResult(t.Context(), cfg, database, false)
				require.NoError(t, err)
				beforeID, err := database.GetDatabaseID(t.Context())
				require.NoError(t, err)
				require.NoError(t, os.Rename(source.Repository, source.Repository+"-offline"))
				t.Cleanup(func() { require.NoError(t, os.Rename(source.Repository+"-offline", source.Repository)) })
				dbtest.WriteTestFile(t, filepath.Join(cfg.AgentDirs[parser.AgentClaude][0], "project", "push-local.jsonl"),
					[]byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "new local message", "push-local").String()))
				if mode.stale {
					require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), "PRAGMA user_version = 0")
						return err
					}))
					require.NoError(t, database.Close())
					database, err = db.OpenIsolated(t.Context(), cfg.DBPath)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, database.Close()) })
					require.True(t, database.NeedsResync())
				}
				if mode.localError {
					require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER fail_local_write BEFORE INSERT ON messages
							WHEN NEW.session_id = 'push-local' BEGIN SELECT RAISE(FAIL, 'injected local write failure'); END`)
						return err
					}))
				}
				probe := &archiveSourcePushProbe{seen: make(chan []db.Message, 1), inspect: func(ctx context.Context, database *db.DB) {
					messages, err := database.GetAllMessages(ctx, "push-local")
					require.NoError(t, err)
					require.Len(t, messages, 1)
					assert.Equal(t, "new local message", messages[0].Content)
				}}
				backend := &localArchiveWriteBackend{
					appCfg: cfg, database: database,
					ensurePricing: func(context.Context, *db.DB) error { return nil },
				}
				if name == "duckdb" {
					original := mirrorBackend
					mirrorBackend = archiveSourcePushMirror{probe: probe}
					t.Cleanup(func() { mirrorBackend = original })
					_, err = backend.DuckDBPush(t.Context(), config.DuckDBConfig{Path: filepath.Join(t.TempDir(), "mirror.duckdb")}, DuckDBPushConfig{Full: mode.full}, nil, nil)
				} else {
					var replica storage.Replica = postgres.Backend{}
					if name == "clickhouse" {
						replica = clickhouse.Backend{}
					}
					_, err = backend.ReplicaPush(t.Context(), archiveSourcePushReplica{Replica: replica, probe: probe}, storage.ConfiguredReplica{}, ReplicaPushConfig{Full: mode.full}, nil, nil)
				}
				if mode.localError {
					require.ErrorContains(t, err, "local sync processing incomplete")
					assert.Empty(t, probe.seen, "local sync failures must stop the destination push")
					return
				}
				require.NoError(t, err)
				messages := receiveArchiveTest(t, probe.seen)
				require.Len(t, messages, 1)
				assert.Equal(t, "archived message", messages[0].Content)
				assert.Equal(t, mode.wantFull, probe.full)
				afterID, err := database.GetDatabaseID(t.Context())
				require.NoError(t, err)
				assert.Equal(t, mode.wantFull, beforeID != afterID, "full pushes and parser upgrades must rebuild the archive")
			})
		}
	}
}

func TestArchiveSourceDirectPushWatchStartup(t *testing.T) {
	source := archiveSourceCommandFixture(t)
	for _, mirror := range []bool{false, true} {
		for _, full := range []bool{false, true} {
			t.Run("mirror="+strconv.FormatBool(mirror)+"/full="+strconv.FormatBool(full), func(t *testing.T) {
				backend := testLocalArchiveWriteBackend(t)
				backend.appCfg.ArchiveSources = []config.ArchiveSource{source}
				if full {
					_, _, err := runLocalSyncResult(t.Context(), backend.appCfg, backend.database, false)
					require.NoError(t, err)
					require.NoError(t, backend.database.Update(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), `UPDATE messages SET content = 'stale parser output' WHERE session_id = ?`, attachedPushSession)
						return err
					}))
				}
				probe := &archiveSourcePushProbe{database: backend.database, seen: make(chan []db.Message, 4)}
				hooks := newPushWatchOwnerHarness().hooks()
				hooks.startupSync = nil
				hooks.startWatcher = nil
				hooks.newReplicaPusher = func(*syncpkg.Engine) *replicaPusher {
					return &replicaPusher{session: archivepush.ReplicaSession{
						Connect: func(context.Context) (storage.Pusher, error) { return probe, nil },
					}}
				}
				hooks.newDuckDBPusher = func(*syncpkg.Engine) *duckDBPusher {
					return &duckDBPusher{mirrorPush: func(ctx context.Context, _ bool) (storage.MirrorPushResult, error) {
						_, err := probe.PushWithOptions(ctx, storage.PushOptions{}, nil)
						return storage.MirrorPushResult{}, err
					}}
				}
				backend.watchHooks = hooks
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() {
					if mirror {
						done <- backend.DuckDBPushWatch(ctx, config.DuckDBConfig{}, DuckDBPushConfig{Full: full}, nil, nil, time.Hour, time.Hour)
					} else {
						done <- backend.ReplicaPushWatch(ctx, pgReplica{}, storage.ConfiguredReplica{}, ReplicaPushConfig{Full: full}, nil, nil, time.Hour, time.Hour)
					}
				}()
				t.Cleanup(func() {
					cancel()
					require.NoError(t, receiveArchiveTest(t, done))
				})
				messages := receiveArchiveTest(t, probe.seen)
				require.Len(t, messages, 1, "watch startup must import attached history before pushing")
				assert.Equal(t, "archived message", messages[0].Content, "--full must reparse available attached sources")
			})
		}
	}
}

func TestArchiveSourcePushWatchPeriodicSyncRetriesAttachments(t *testing.T) {
	source := archiveSourceCommandFixture(t)
	backend := testLocalArchiveWriteBackend(t)
	backend.appCfg.ArchiveSources = []config.ArchiveSource{source}
	engine, err := backend.startWatchEngine(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(engine.Close)
	manager, err := archivesource.NewManager(t.Context(), backend.appCfg, backend.database, engine, nil)
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	loop, stop := newArchivePushLoop(nil, "test watch", time.Hour, time.Hour, nil)
	t.Cleanup(stop)
	lane := archiveSyncLane{label: "test watch", engine: engine, archiveSources: manager, loop: loop}

	// An attachment unavailable on one periodic pass must be retried by the
	// next pass, even though no local session has changed.
	require.NoError(t, os.Rename(source.Repository, source.Repository+"-offline"))
	lane.syncAll(t.Context())
	messages, err := backend.database.GetAllMessages(t.Context(), attachedPushSession)
	require.NoError(t, err)
	assert.Empty(t, messages)
	pending, _ := pushLoopPendingState(loop)
	assert.False(t, pending)

	require.NoError(t, os.Rename(source.Repository+"-offline", source.Repository))
	lane.syncAll(t.Context())
	messages, err = backend.database.GetAllMessages(t.Context(), attachedPushSession)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "archived message", messages[0].Content)
	pending, _ = pushLoopPendingState(loop)
	assert.True(t, pending, "imported history must queue a push without a local file event")
}

func TestArchiveSourcePushWatchStartupAllowsOfflineHistory(t *testing.T) {
	source := archiveSourceCommandFixture(t)
	backend := testLocalArchiveWriteBackend(t)
	backend.appCfg.ArchiveSources = []config.ArchiveSource{source}
	_, _, err := runLocalSyncResult(t.Context(), backend.appCfg, backend.database, false)
	require.NoError(t, err)
	require.NoError(t, os.Rename(source.Repository, source.Repository+"-offline"))
	engine, err := backend.startWatchEngine(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(engine.Close)
	manager, err := archivesource.NewManager(t.Context(), backend.appCfg, backend.database, engine, nil)
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	id, err := backend.database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	lane := archiveSyncLane{
		label: "test watch", engine: engine, database: backend.database,
		archiveSources: manager, startupSync: runPGWatchStartupSync,
		startDatabaseID: id, openDispatch: func() {}, retry: time.Hour,
	}
	require.True(t, lane.syncStartup(t.Context()), "offline history must not pause pushes from a current archive")
	called := false
	require.NoError(t, lane.push(t.Context(), reasonStartup, nil, func(ctx context.Context, _ pushReason, _ bool, _ *syncpkg.WatchBatch) error {
		called = true
		messages, err := backend.database.GetAllMessages(ctx, attachedPushSession)
		require.NoError(t, err)
		require.Len(t, messages, 1)
		assert.Equal(t, "archived message", messages[0].Content)
		return nil
	}))
	assert.True(t, called)
}

func TestArchiveSourcePushWatchPublishesWritesBeforeLinkFailure(t *testing.T) {
	source := archiveSourceCommandFixture(t)
	backend := testLocalArchiveWriteBackend(t)
	backend.appCfg.ArchiveSources = []config.ArchiveSource{source}
	// A previously indexed parent points at a child in the new attachment.
	require.NoError(t, backend.database.UpsertSession(t.Context(), db.Session{ID: "parent", Agent: "claude", Machine: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}))
	require.NoError(t, backend.database.InsertMessages(t.Context(), []db.Message{{
		SessionID: "parent", Ordinal: 0, Role: "assistant", Content: "spawn child", HasToolUse: true,
		ToolCalls: []db.ToolCall{{ToolUseID: "spawn", ToolName: "Task", SubagentSessionID: attachedPushSession}},
	}}))
	// Fail the separate linking transaction, after the transcript commits.
	require.NoError(t, backend.database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER fail_attached_link
			BEFORE UPDATE OF parent_session_id ON sessions
			WHEN NEW.id = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa~saved' AND NEW.parent_session_id = 'parent'
			BEGIN SELECT RAISE(FAIL, 'injected attached link failure'); END`)
		return err
	}))
	emitter := &scopedEmitter{scopes: make(chan string, 8)}
	engine, err := backend.startWatchEngine(t.Context(), emitter)
	require.NoError(t, err)
	t.Cleanup(engine.Close)
	manager, err := archivesource.NewManager(t.Context(), backend.appCfg, backend.database, engine, nil)
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	loop, stop := newArchivePushLoop(nil, "test watch", time.Hour, time.Hour, nil)
	t.Cleanup(stop)
	lane := archiveSyncLane{label: "test watch", engine: engine, archiveSources: manager, loop: loop}
	lane.syncAll(t.Context())
	messages, err := backend.database.GetAllMessages(t.Context(), attachedPushSession)
	require.NoError(t, err)
	require.Len(t, messages, 1, "the transcript must have committed before linking failed")
	assert.Equal(t, "archived message", messages[0].Content)
	var pending int
	var lastError string
	require.NoError(t, backend.database.Reader().QueryRow(t.Context(), `SELECT pending_count FROM archive_attachments`).Scan(&pending))
	assert.Equal(t, 1, pending, "post-write failure must leave the source pending")
	require.NoError(t, backend.database.Reader().QueryRow(t.Context(), `SELECT last_error FROM archive_attachment_sources`).Scan(&lastError))
	assert.Contains(t, lastError, "injected attached link failure")
	queued, _ := pushLoopPendingState(loop)
	assert.True(t, queued, "committed history must queue a push even while linking is pending")
	var scopes []string
	for len(emitter.scopes) > 0 {
		scopes = append(scopes, <-emitter.scopes)
	}
	assert.Contains(t, scopes, "sessions", "consumers must hear about the committed transcript")

	require.NoError(t, backend.database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DROP TRIGGER fail_attached_link`)
		return err
	}))
	lane.syncAll(t.Context())
	require.NoError(t, backend.database.Reader().QueryRow(t.Context(), `SELECT pending_count FROM archive_attachments`).Scan(&pending))
	assert.Zero(t, pending, "the next periodic sync must complete the pending source")
}
