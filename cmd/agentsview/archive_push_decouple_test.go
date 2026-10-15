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
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// blockedRemotePush stands in for an hour-long full push: it reports that it
// started and holds until the test releases it or its context ends.
type blockedRemotePush struct {
	started chan struct{}
	release chan struct{}
}

func newBlockedRemotePush() *blockedRemotePush {
	return &blockedRemotePush{
		started: make(chan struct{}, 8),
		release: make(chan struct{}),
	}
}

func (b *blockedRemotePush) wait(ctx context.Context) error {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*blockedRemotePush) EnsureSchema(context.Context) error { return nil }
func (b *blockedRemotePush) PushWithOptions(
	ctx context.Context, _ storage.PushOptions, _ func(storage.PushProgress),
) (storage.PushResult, error) {
	return storage.PushResult{}, b.wait(ctx)
}
func (*blockedRemotePush) Close() error { return nil }

// A remote push that never finishes must not stop the local archive from
// ingesting a session file written while the push runs.
func TestLocalPushWatchIngestsWhileRemotePushBlocks(t *testing.T) {
	owners := []struct {
		name string
		run  func(context.Context, *localArchiveWriteBackend, *blockedRemotePush) error
	}{
		{
			name: "replica",
			run: func(
				ctx context.Context, b *localArchiveWriteBackend, remote *blockedRemotePush,
			) error {
				b.watchHooks = &archivePushWatchHooks{
					newReplicaPusher: func(*syncpkg.Engine) *replicaPusher {
						return &replicaPusher{
							label: "clickhouse watch", displayName: "ClickHouse",
							connect: func(context.Context) (storage.Pusher, error) {
								return remote, nil
							},
							vectorReconcileNeeded: true,
						}
					},
				}
				return b.ReplicaPushWatch(
					ctx, pgReplica{}, storage.ConfiguredReplica{}, ReplicaPushConfig{},
					nil, nil, time.Hour, time.Hour,
				)
			},
		},
		{
			name: "duckdb",
			run: func(
				ctx context.Context, b *localArchiveWriteBackend, remote *blockedRemotePush,
			) error {
				b.watchHooks = &archivePushWatchHooks{
					newDuckDBPusher: func(*syncpkg.Engine) *duckDBPusher {
						return &duckDBPusher{
							mirrorPush: func(
								c context.Context, _ bool,
							) (storage.MirrorPushResult, error) {
								return storage.MirrorPushResult{}, remote.wait(c)
							},
						}
					},
				}
				return b.DuckDBPushWatch(
					ctx, config.DuckDBConfig{}, DuckDBPushConfig{}, nil, nil,
					time.Hour, time.Hour,
				)
			},
		},
	}
	for _, owner := range owners {
		t.Run(owner.name, func(t *testing.T) {
			dataDir := t.TempDir()
			dbPath := filepath.Join(dataDir, "sessions.db")
			database := dbtest.OpenTestDBAt(t, dbPath)
			codexRoot := t.TempDir()
			backend := &localArchiveWriteBackend{
				appCfg: config.Config{
					DataDir:        dataDir,
					DBPath:         dbPath,
					InstallationID: "local",
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentCodex: {codexRoot},
					},
				},
				database: database,
			}
			remote := newBlockedRemotePush()

			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() { done <- owner.run(ctx, backend, remote) }()

			select {
			case <-remote.started:
			case err := <-done:
				require.FailNowf(t, "watch exited before the startup push", "%v", err)
			case <-time.After(10 * time.Second):
				require.FailNow(t, "startup push never began")
			}

			uuid := "a1b2c3d4-1111-4222-8333-444455556666"
			day := filepath.Join(codexRoot, "2026", "10", "15")
			require.NoError(t, os.MkdirAll(day, 0o755))
			content := testjsonl.NewSessionBuilder().
				AddCodexMeta(
					"2026-10-15T14:00:00Z", uuid, "/home/user/code/api",
					"codex_cli_rs",
				).
				AddCodexMessage("2026-10-15T14:00:01Z", "user", "hello").
				String()
			require.NoError(t, os.WriteFile(
				filepath.Join(day, "rollout-2026-10-15T14-00-00-"+uuid+".jsonl"),
				[]byte(content), 0o644,
			))

			require.Eventually(t, func() bool {
				session, err := database.GetSession(t.Context(), "codex:"+uuid)
				return err == nil && session != nil
			}, 10*time.Second, 20*time.Millisecond,
				"a session written during a blocked remote push must reach the archive")

			close(remote.release)
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(30 * time.Second):
				require.FailNow(t, "watch did not shut down")
			}
		})
	}
}

// Local owners open watcher dispatch as soon as startup sync finishes, so the
// startup push and its retries never hold back watcher batches.
func TestLocalPushWatchOpensDispatchBeforeStartupPush(t *testing.T) {
	for _, owner := range localPushWatchOwnerCases(t) {
		t.Run(owner.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newPushWatchOwnerHarness(1)
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				done := make(chan error, 1)
				go func() { done <- owner.run(ctx, h.hooks()) }()

				receiveArchiveTest(t, h.opened)
				first := receiveArchiveTest(t, h.attempts)
				require.Equal(t, 1, first.index)
				synctest.Wait()
				events, opens, _ := h.snapshot()
				assert.Equal(t, 1, opens)
				assert.Less(t, indexOfEvent(events, "startup-sync"),
					indexOfEvent(events, "open"))
				assert.Less(t, indexOfEvent(events, "open"),
					indexOfEvent(events, "push"))
				pending, waiters := h.pending()
				require.True(t, pending && waiters == 0,
					"a partial startup push stays queued for retry")

				// An authoritative batch syncs locally and returns without
				// waiting for the push it queues.
				require.NoError(t, h.watcherCallback()(
					ctx, syncpkg.WatchBatch{FullSync: true},
				))

				h.floor <- time.Now()
				second := receiveArchiveTest(t, h.attempts)
				require.Equal(t, 2, second.index)
				synctest.Wait()
				pending, _ = h.pending()
				assert.False(t, pending, "the retry covers the queued batch")

				cancel()
				require.NoError(t, receiveArchiveTest(t, done))
			})
		})
	}
}

// When startup sync fails, local owners keep dispatch closed and skip pushes
// until a periodic local sync completes. The first pushes after that resend
// every session when startup rebuilt the archive, until one succeeds.
func TestLocalPushWatchHoldsPushesUntilLocalSyncCompletes(t *testing.T) {
	for _, owner := range localPushWatchOwnerCases(t) {
		t.Run(owner.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newPushWatchOwnerHarness(1)
				hooks := h.hooks()
				failedStartup := func(
					context.Context, *syncpkg.Engine, bool,
				) (bool, error) {
					h.record("startup-sync")
					return true, errors.New("startup sync discovery incomplete")
				}
				hooks.replicaStartupSync = failedStartup
				hooks.duckDBStartupSync = failedStartup
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				done := make(chan error, 1)
				go func() { done <- owner.run(ctx, hooks) }()

				synctest.Wait()
				h.floor <- time.Now()
				synctest.Wait()
				_, opens, _ := h.snapshot()
				require.Zero(t, opens, "dispatch stays closed until a local sync completes")
				select {
				case attempt := <-h.attempts:
					require.Failf(t, "pushed before a complete local sync",
						"attempt %d", attempt.index)
				default:
				}

				// The local sync lane runs on the interval.
				time.Sleep(time.Hour)
				synctest.Wait()
				receiveArchiveTest(t, h.opened)

				h.fire <- time.Now()
				first := receiveArchiveTest(t, h.attempts)
				assert.True(t, first.full, "the startup resync makes the first push full")
				synctest.Wait()

				h.floor <- time.Now()
				retry := receiveArchiveTest(t, h.attempts)
				assert.False(t, retry.full,
					"the mirror resumes a failed full push, so a retry must not start it over")

				cancel()
				require.NoError(t, receiveArchiveTest(t, done))
			})
		})
	}
}

// A periodic local sync queues a push only when it writes the archive, or
// when it is the first complete sync and startup never pushed.
func TestArchiveSyncLaneQueuesPushOnlyAfterArchiveChanges(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	codexRoot := t.TempDir()
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {codexRoot}},
	})
	t.Cleanup(engine.Close)
	lane := &archiveSyncLane{
		label: "test watch", engine: engine, openDispatch: func() {},
	}
	newLoop := func() {
		loop, ticker := newPushLoopWithLabel("test watch", time.Hour, time.Hour,
			func(context.Context, pushReason, *syncpkg.WatchBatch) error { return nil })
		t.Cleanup(ticker.Stop)
		lane.loop = loop
	}
	queued := func() bool {
		pending, _ := pushLoopPendingState(lane.loop)
		return pending
	}
	ctx := t.Context()

	newLoop()
	lane.syncAll(ctx)
	assert.True(t, queued(), "the first complete sync queues the skipped push")

	newLoop()
	lane.syncAll(ctx)
	assert.False(t, queued(), "a sync that wrote nothing queues no push")

	uuid := "b2c3d4e5-2222-4333-8444-555566667777"
	day := filepath.Join(codexRoot, "2026", "10", "15")
	require.NoError(t, os.MkdirAll(day, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(day, "rollout-2026-10-15T15-00-00-"+uuid+".jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddCodexMeta("2026-10-15T15:00:00Z", uuid, "/home/user/code/api", "codex_cli_rs").
			AddCodexMessage("2026-10-15T15:00:01Z", "user", "hello").
			String()),
		0o644,
	))
	lane.syncAll(ctx)
	assert.True(t, queued(), "a sync that wrote a session queues a push")
}
