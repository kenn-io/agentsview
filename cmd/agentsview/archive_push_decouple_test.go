package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/archivepush"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/agentsview/internal/testutil"
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
							session: archivepush.ReplicaSession{
								Connect: func(context.Context) (storage.Pusher, error) {
									return remote, nil
								},
							},
							vectors: newVectorReconcile(),
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

// When startup sync fails, local owners keep dispatch closed, skip pushes,
// and retry the startup sync, with its --full rebuild, until one completes.
// The --full request survives the failure: the first push after that resends
// every session, and later pushes do not.
func TestLocalPushWatchHoldsPushesUntilLocalSyncCompletes(t *testing.T) {
	for _, owner := range localPushWatchOwnerCases(t) {
		t.Run(owner.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newPushWatchOwnerHarness(1)
				hooks := h.hooks()
				var attempts atomic.Int32
				hooks.startupSync = func(
					_ context.Context, _ *syncpkg.Engine, full bool,
				) (bool, error) {
					h.record("startup-sync")
					assert.True(t, full, "each startup sync attempt must get the --full request")
					if attempts.Add(1) == 1 {
						return false, errors.New("startup sync discovery incomplete")
					}
					return false, nil
				}
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				done := make(chan error, 1)
				go func() { done <- owner.runFull(ctx, hooks) }()

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

				// The local sync lane retries on the debounce, well before the
				// floor interval.
				time.Sleep(localPushWatchTestDebounce)
				synctest.Wait()
				receiveArchiveTest(t, h.opened)
				assert.Equal(t, int32(2), attempts.Load(),
					"the retry reruns the startup sync, so a rebuild it needs still runs")

				h.fire <- time.Now()
				first := receiveArchiveTest(t, h.attempts)
				assert.True(t, first.full, "--full makes the first push full")
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

// A startup rebuild can replace the archive and still report failures. The
// watch must then make its first push full, because only a full push removes
// remote rows that the rebuild dropped. A --full retry must not rebuild the
// archive a second time.
func TestLocalPushWatchPushesFullAfterStartupRebuildWithFailures(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, owner := range localPushWatchOwnerCases(t) {
			t.Run(fmt.Sprintf("%s full=%t", owner.name, full), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					h := newPushWatchOwnerHarness()
					hooks := h.hooks()
					var fullArgs []bool
					hooks.startupSync = func(
						ctx context.Context, _ *syncpkg.Engine, fullArg bool,
					) (bool, error) {
						fullArgs = append(fullArgs, fullArg)
						if len(fullArgs) == 1 {
							require.NoError(t, owner.database.SetDatabaseIDForTest(ctx, "rebuilt"))
							return false, errors.New("startup sync processing incomplete")
						}
						return false, nil
					}
					run := owner.run
					if full {
						run = owner.runFull
					}
					ctx, cancel := context.WithCancel(t.Context())
					t.Cleanup(cancel)
					done := make(chan error, 1)
					go func() { done <- run(ctx, hooks) }()

					synctest.Wait()
					time.Sleep(localPushWatchTestDebounce)
					synctest.Wait()
					receiveArchiveTest(t, h.opened)
					h.fire <- time.Now()
					first := receiveArchiveTest(t, h.attempts)
					assert.True(t, first.full, "the first push after a rebuild must be full")
					assert.Equal(t, []bool{full, false}, fullArgs,
						"a retry after a rebuild must not rebuild again")

					cancel()
					require.NoError(t, receiveArchiveTest(t, done))
				})
			})
		}
	}
}

// A retry of the startup sync can rebuild the whole archive, so repeated
// failures space the retries out: the wait doubles each time, up to the floor
// interval.
func TestLocalPushWatchBacksOffFailedStartupSyncs(t *testing.T) {
	for _, owner := range localPushWatchOwnerCases(t) {
		t.Run(owner.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newPushWatchOwnerHarness()
				hooks := h.hooks()
				start := time.Now()
				var attempts []time.Duration
				hooks.startupSync = func(context.Context, *syncpkg.Engine, bool) (bool, error) {
					attempts = append(attempts, time.Since(start))
					return false, errors.New("startup sync discovery incomplete")
				}
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				done := make(chan error, 1)
				go func() { done <- owner.run(ctx, hooks) }()

				time.Sleep(7*localPushWatchTestDebounce + time.Second)
				synctest.Wait()
				d := localPushWatchTestDebounce
				assert.Equal(t, []time.Duration{0, d, 3 * d, 7 * d}, attempts)

				cancel()
				require.NoError(t, receiveArchiveTest(t, done))
			})
		})
	}
}

// A periodic local sync queues a push only when it writes the archive.
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
	lane.ready.Store(true)
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
	// The first sync of a new archive records its baseline.
	lane.syncAll(ctx)

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

// Stopping a local watch archives changes the watcher had not dispatched yet,
// so the final push includes them.
func TestArchiveSyncLaneSyncsBeforeFinalPush(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	codexRoot := t.TempDir()
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {codexRoot}},
	})
	t.Cleanup(engine.Close)
	lane := &archiveSyncLane{
		label: "test watch", engine: engine, openDispatch: func() {},
	}
	lane.ready.Store(true)
	uuid := "c3d4e5f6-3333-4444-8555-666677778888"
	day := filepath.Join(codexRoot, "2026", "10", "16")
	require.NoError(t, os.MkdirAll(day, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(day, "rollout-2026-10-16T09-00-00-"+uuid+".jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddCodexMeta("2026-10-16T09:00:00Z", uuid, "/home/user/code/api", "codex_cli_rs").
			AddCodexMessage("2026-10-16T09:00:01Z", "user", "hello").
			String()),
		0o644,
	))

	var archived int
	require.NoError(t, lane.push(t.Context(), reasonShutdown, nil, func(
		ctx context.Context, _ pushReason, _ bool, _ *syncpkg.WatchBatch,
	) error {
		var err error
		archived, err = database.CountSessionsForMirrorScope(ctx, nil, nil)
		return err
	}))
	assert.Equal(t, 1, archived, "the final push must see the pending session")
}

// Stopping a local watch during its first push skips the final push but
// still archives changes the watcher had not handed over.
func TestLocalPushWatchSyncsWhenStoppedDuringStartupPush(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)
	codexRoot := t.TempDir()
	backend.appCfg.AgentDirs = map[parser.AgentType][]string{parser.AgentCodex: {codexRoot}}
	h := newPushWatchOwnerHarness()
	hooks := h.hooks()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var pushes atomic.Int32
	hooks.newDuckDBPusher = func(*syncpkg.Engine) *duckDBPusher {
		return &duckDBPusher{mirrorPush: func(
			pctx context.Context, _ bool,
		) (storage.MirrorPushResult, error) {
			if pushes.Add(1) > 1 {
				assert.Fail(t, "stopping during the first push must not push again")
				return storage.MirrorPushResult{}, nil
			}
			// A session changes while the first push runs, and the
			// watcher has not handed it over when the watch stops.
			uuid := "d4e5f6a7-4444-4555-8666-777788889999"
			day := filepath.Join(codexRoot, "2026", "10", "17")
			require.NoError(t, os.MkdirAll(day, 0o755))
			require.NoError(t, os.WriteFile(
				filepath.Join(day, "rollout-2026-10-17T09-00-00-"+uuid+".jsonl"),
				[]byte(testjsonl.NewSessionBuilder().
					AddCodexMeta("2026-10-17T09:00:00Z", uuid, "/home/user/code/api", "codex_cli_rs").
					AddCodexMessage("2026-10-17T09:00:01Z", "user", "hello").
					String()),
				0o644,
			))
			cancel()
			<-pctx.Done()
			return storage.MirrorPushResult{}, pctx.Err()
		}}
	}
	backend.watchHooks = hooks

	require.NoError(t, backend.DuckDBPushWatch(
		ctx, config.DuckDBConfig{}, DuckDBPushConfig{}, nil, nil, time.Hour, time.Hour,
	))
	archived, err := backend.database.CountSessionsForMirrorScope(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, archived, "the stopped watch must archive the pending session")
}

// A push waits for an in-progress local sync, so it never copies a session
// that the sync has only half written.
func TestArchiveSyncLaneSettlesBeforePush(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{})
	t.Cleanup(engine.Close)
	lane := &archiveSyncLane{
		label: "test watch", engine: engine, openDispatch: func() {},
	}
	lane.ready.Store(true)

	held := make(chan struct{})
	release := make(chan struct{})
	releaseSync := sync.OnceFunc(func() { close(release) })
	// Runs before the engine closes, which waits for the held sync.
	t.Cleanup(releaseSync)
	syncDone := make(chan error, 1)
	go func() {
		syncDone <- engine.RunExclusive(func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	var pushed atomic.Bool
	pushDone := make(chan error, 1)
	go func() {
		pushDone <- lane.push(t.Context(), reasonChange, nil, func(
			context.Context, pushReason, bool, *syncpkg.WatchBatch,
		) error {
			pushed.Store(true)
			return nil
		})
	}()
	require.Eventually(t, func() bool {
		return pushed.Load() || testutil.GoroutineBlockedOnMutexIn("(*Engine).SettleForPush")
	}, 10*time.Second, 5*time.Millisecond)
	require.False(t, pushed.Load(), "a push must wait for an in-progress sync")

	releaseSync()
	require.NoError(t, <-syncDone)
	require.NoError(t, <-pushDone)
	assert.True(t, pushed.Load())
}

// A --full push that fails before it reaches the destination recorded no
// full push to resume, so the next push stays full. A push that fails after
// it started resumes on its own, so asking for full again would restart it.
func TestArchiveSyncLaneKeepsFullUntilAPushReachesTheDestination(t *testing.T) {
	lane := &archiveSyncLane{label: "test watch", openDispatch: func() {}}
	lane.fullPending.Store(true)
	var fullArgs []bool
	push := func(results ...error) {
		for _, result := range results {
			err := lane.push(t.Context(), reasonChange, nil, func(
				_ context.Context, _ pushReason, full bool, _ *syncpkg.WatchBatch,
			) error {
				fullArgs = append(fullArgs, full)
				return result
			})
			require.ErrorIs(t, err, result)
		}
	}
	unreachable := errors.New("daemon unreachable")
	copyFailed := errors.New("copy failed")
	push(storage.PushNotStartedError{Err: unreachable}, copyFailed, copyFailed)
	assert.Equal(t, []bool{true, true, false}, fullArgs)
}

// Shutdown stops the watcher before the final push. A watcher batch can hold
// the sync lock until the watcher cancels it, and the final push waits for
// that lock, so stopping the watcher afterwards would stall shutdown.
func TestLocalPushWatchStopsWatcherBeforeFinalPush(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	callbackCtx, stopCallbacks := context.WithCancel(t.Context())
	t.Cleanup(stopCallbacks)
	engines := make(chan *syncpkg.Engine, 1)
	pushed := make(chan struct{}, 2)
	idle := make(chan struct{}, 1)
	target := &fakeTarget{onPush: func() {
		record("push")
		pushed <- struct{}{}
	}}
	backend.watchHooks = &archivePushWatchHooks{
		startWatcher: func(
			config.Config, *syncpkg.Engine, syncpkg.WatchCallback,
			syncpkg.WatcherOptions,
		) (func(), func(), []string) {
			return func() {
				record("watcher stopped")
				stopCallbacks()
			}, func() {}, nil
		},
		startupSync: func(
			_ context.Context, engine *syncpkg.Engine, _ bool,
		) (bool, error) {
			engines <- engine
			return false, nil
		},
		newReplicaPusher: func(*syncpkg.Engine) *replicaPusher {
			return &replicaPusher{
				session: archivepush.ReplicaSession{
					Connect: func(context.Context) (storage.Pusher, error) {
						return target, nil
					},
				},
				vectors: newVectorReconcile(),
			}
		},
		newLoop: func(
			label string, debounce, interval time.Duration,
			push func(context.Context, pushReason, *syncpkg.WatchBatch) error,
		) (*pushLoop, func()) {
			loop, ticker := newPushLoopWithLabel(label, debounce, interval, push)
			loop.onIdle = func() {
				select {
				case idle <- struct{}{}:
				default:
				}
			}
			return loop, ticker.Stop
		},
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- backend.ReplicaPushWatch(
			ctx, pgReplica{}, storage.ConfiguredReplica{}, ReplicaPushConfig{},
			nil, nil, time.Hour, time.Hour,
		)
	}()
	engine := <-engines
	<-pushed
	// Cancelling during the startup push skips the final push, so wait for
	// the loop to start waiting for work.
	<-idle

	// Stand in for a watcher batch that syncs until the watcher stops it.
	held := make(chan struct{})
	batchDone := make(chan error, 1)
	go func() {
		batchDone <- engine.RunExclusive(func() error {
			close(held)
			<-callbackCtx.Done()
			return nil
		})
	}()
	<-held

	cancel()
	require.NoError(t, <-done)
	require.NoError(t, <-batchDone)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"push", "watcher stopped", "push"}, events,
		"the final push runs after the watcher stops")
}
