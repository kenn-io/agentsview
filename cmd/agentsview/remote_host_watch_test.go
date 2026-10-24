package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/archivepush"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestReplicaWatchPullsRemoteHostsAndJoinsWorkers(t *testing.T) {
	for _, embed := range []bool{false, true} {
		t.Run(fmt.Sprintf("embed=%v", embed), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			dir := t.TempDir()
			cfg := config.Config{DataDir: dir, DBPath: database.Path(), AgentDirs: map[parser.AgentType][]string{}, RemoteHosts: []config.RemoteHost{
				{Host: "first", Transport: config.RemoteTransportSSH, Interval: 10 * time.Millisecond, SyncTimeout: time.Minute},
				{Host: "manual", Transport: config.RemoteTransportSSH},
				{Host: "second", Transport: config.RemoteTransportSSH, Interval: 10 * time.Millisecond, SyncTimeout: time.Minute},
			}}
			for _, host := range cfg.RemoteHosts {
				require.NoError(t, database.SetRemoteImportDataVersion(t.Context(), host.Host, db.CurrentDataVersion()))
				require.NoError(t, os.MkdirAll(remotesync.MirrorDir(dir, host.Host), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(remotesync.MirrorDir(dir, host.Host), "session"), []byte("session"), 0o600))
			}
			if embed {
				cfg.Vector = vectorTestConfig(dir).Vector
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			old := runRemoteHostSync
			t.Cleanup(func() { runRemoteHostSync = old })
			startupPush := make(chan struct{})
			var calls atomic.Int64
			started := make(chan struct{})
			canceled := make(chan struct{})
			release := make(chan struct{})
			releaseWorker := stdsync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseWorker)
			dirty := make(chan struct{}, 1)
			var startupComplete atomic.Bool
			runRemoteHostSync = func(c context.Context, _ config.Config, d *db.DB, h config.RemoteHost, full bool) (remotesync.SyncStats, error) {
				assert.NotEqual(t, "manual", h.Host, "zero interval must remain manual-only")
				assert.False(t, full, "--full is startup only")
				if !startupComplete.Load() {
					deadline, bounded := c.Deadline()
					assert.True(t, bounded, "incremental startup must apply sync_timeout")
					assert.InDelta(t, 60, time.Until(deadline).Seconds(), 1)
					require.NoError(t, d.UpsertSession(c, db.Session{ID: h.Host + "-startup", Project: "remote", Agent: "claude", Machine: h.Host, MessageCount: 1}))
					return remotesync.SyncStats{SessionsSynced: 1}, nil
				}
				// Workers now start before the initial push. Keep those first
				// polls empty so this fixture commits a change after its cutoff.
				select {
				case <-startupPush:
				default:
					return remotesync.SyncStats{}, nil
				}
				n := calls.Add(1)
				if n == 1 {
					require.NoError(t, d.UpsertSession(c, db.Session{ID: "periodic", Project: "remote", Agent: "claude", Machine: h.Host, MessageCount: 1}))
					return remotesync.SyncStats{SessionsSynced: 1}, errors.New("partial import")
				}
				if n == 2 {
					close(started)
					<-c.Done()
					// The loop reports the error after releasing the import lock.
					// Hold that worker at its error boundary so final local sync
					// cannot stand in for the worker join.
					return remotesync.SyncStats{}, heldRemoteCancellation{report: func() {
						close(canceled)
						<-release
					}}
				}
				return remotesync.SyncStats{}, c.Err()
			}
			var queued *embedScheduler
			replica := &embedPushReplica{}
			onPush := func(c context.Context) {
				if c.Err() != nil {
					return
				}
				select {
				case <-startupPush:
					return
				default:
				}

				if embed {
					embedder := replica.source.(*replicaEmbedder)
					// Observe only work queued after startup, without a consumer racing it.
					queued = newEmbedScheduler(nil, time.Hour, 0, false, nil)
					embedder.emitterMu.Lock()
					embedder.scheduler = queued
					embedder.emitterMu.Unlock()
				}
				close(startupPush)
			}
			backend := &localArchiveWriteBackend{appCfg: cfg, database: database, watchHooks: &archivePushWatchHooks{
				newLoop: func(label string, debounce, interval time.Duration, push func(context.Context, pushReason, *syncpkg.WatchBatch) error) (*pushLoop, func()) {
					loop, ticker := newPushLoopWithLabel(label, debounce, interval, push)
					loop.after = func(time.Duration) <-chan time.Time {
						select {
						case dirty <- struct{}{}:
						default:
						}
						return nil
					}
					return loop, ticker.Stop
				},
				startWatcher: func(_ config.Config, _ *syncpkg.Engine, _ syncpkg.WatchCallback, _ syncpkg.WatcherOptions) (func(), func(), []string) {
					sessions, err := database.ListSessions(ctx, db.SessionFilter{})
					require.NoError(t, err)
					require.Len(t, sessions.Sessions, 2, "both startup pulls finish before lane engine watch")
					startupComplete.Store(true)
					return func() {}, func() {}, nil
				},
				newReplicaPusher: func(*syncpkg.Engine) *replicaPusher {
					return &replicaPusher{session: archivepush.ReplicaSession{Connect: func(context.Context) (storage.Pusher, error) {
						return remoteWatchPusher{push: onPush}, nil
					}}}
				},
			}}
			var destination storage.Replica = pgReplica{}
			if embed {
				destination = replica
				backend.watchHooks.newReplicaPusher = nil
				replica.beforeExport = func() error { onPush(ctx); return nil }
			}
			done := make(chan error, 1)
			finished := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				releaseWorker()
				select {
				case <-finished:
				case <-time.After(30 * time.Second):
					assert.Fail(t, "watcher cleanup did not join workers")
				}
			})
			go func() {
				defer close(finished)
				done <- backend.ReplicaPushWatch(ctx, destination, embedTarget(), ReplicaPushConfig{Embed: embed}, nil, nil, time.Millisecond, time.Hour)
			}()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				require.FailNow(t, "periodic workers did not start")
			}
			select {
			case <-dirty:
			case <-time.After(10 * time.Second):
				require.FailNow(t, "committed remote changes did not mark the push lane dirty")
			}
			if embed {
				select {
				case release := <-queued.dirty:
					release()
				case <-time.After(10 * time.Second):
					require.FailNow(t, "partial remote import did not queue embeddings")
				}
			}
			// Hold the canceled import so cancellation alone cannot satisfy the join.
			cancel()
			select {
			case <-canceled:
			case <-time.After(10 * time.Second):
				require.FailNow(t, "remote worker did not observe cancellation")
			}
			select {
			case <-finished:
				assert.Fail(t, "watch returned before the canceled worker was released")
			//nolint:kennlint // This timer proves shutdown cannot finish while the canceled worker is held.
			case <-time.After(100 * time.Millisecond):
			}
			releaseWorker()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				require.FailNow(t, "watcher did not join cancelled imports")
			}
			assert.GreaterOrEqual(t, calls.Load(), int64(2))
		})
	}
}

type remoteWatchPusher struct{ push func(context.Context) }

func (p remoteWatchPusher) EnsureSchema(context.Context) error { return nil }
func (p remoteWatchPusher) PushWithOptions(c context.Context, _ storage.PushOptions, _ func(storage.PushProgress)) (storage.PushResult, error) {
	p.push(c)
	return storage.PushResult{}, nil
}
func (remoteWatchPusher) Close() error { return nil }

func TestReplicaWatchSelectsAutomaticHosts(t *testing.T) {
	for _, tc := range []struct {
		name             string
		destination      storage.Replica
		full, manualOnly bool
		want             []string
	}{
		{name: "PostgreSQL incremental", destination: pgReplica{}, want: []string{"scheduled"}},
		{name: "PostgreSQL full", destination: pgReplica{}, full: true, want: []string{"scheduled"}},
		{name: "PostgreSQL manual only", destination: pgReplica{}, manualOnly: true},
		{name: "ClickHouse", destination: clickhouse.Backend{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			cfg := config.Config{DataDir: t.TempDir(), DBPath: database.Path(), AgentDirs: map[parser.AgentType][]string{}, RemoteHosts: []config.RemoteHost{
				{Host: "manual", Transport: config.RemoteTransportSSH},
			}}
			if !tc.manualOnly {
				cfg.RemoteHosts = append(cfg.RemoteHosts, config.RemoteHost{Host: "scheduled", Transport: config.RemoteTransportSSH, Interval: time.Hour})
			}
			missingPath := filepath.Join(t.TempDir(), "missing.jsonl")
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "archived-manual", Project: "remote", Agent: "claude", Machine: "manual", FilePath: &missingPath, MessageCount: 1}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			oldSync, oldPrepare := runRemoteHostSync, prepareHTTPRebuildCLI
			t.Cleanup(func() { runRemoteHostSync = oldSync; prepareHTTPRebuildCLI = oldPrepare })
			var pulled []string
			runRemoteHostSync = func(c context.Context, _ config.Config, d *db.DB, h config.RemoteHost, _ bool) (remotesync.SyncStats, error) {
				pulled = append(pulled, h.Host)
				require.NoError(t, d.UpsertSession(c, db.Session{ID: h.Host + "-session", Project: "remote", Agent: "claude", Machine: h.Host, MessageCount: 1}))
				return remotesync.SyncStats{SessionsSynced: 1}, nil
			}
			prepareHTTPRebuildCLI = func(_ context.Context, syncs []remotesync.RemoteSync) (preparedHTTPRebuildCLI, error) {
				for _, remote := range syncs {
					pulled = append(pulled, remote.Host)
				}
				return &fakeCLIPreparedHTTPRebuild{}, nil
			}
			backend := &localArchiveWriteBackend{appCfg: cfg, database: database, watchHooks: &archivePushWatchHooks{
				startWatcher: func(config.Config, *syncpkg.Engine, syncpkg.WatchCallback, syncpkg.WatcherOptions) (func(), func(), []string) {
					cancel()
					return func() {}, func() {}, nil
				},
			}}
			require.NoError(t, backend.ReplicaPushWatch(ctx, tc.destination, embedTarget(), ReplicaPushConfig{Full: tc.full}, nil, nil, time.Second, time.Hour))
			assert.Equal(t, tc.want, pulled)
			session, err := database.GetSession(t.Context(), "manual-session")
			require.NoError(t, err)
			assert.Nil(t, session, "watch startup must not fetch a manual host")
			preserved, err := database.GetSession(t.Context(), "archived-manual")
			require.NoError(t, err)
			assert.NotNil(t, preserved, "a full rebuild must preserve archived manual-host sessions")
		})
	}
}

// heldRemoteCancellation pauses failure reporting after the import has ended.
type heldRemoteCancellation struct{ report func() }

func (e heldRemoteCancellation) Error() string {
	e.report()
	return context.Canceled.Error()
}
func (heldRemoteCancellation) Unwrap() error { return context.Canceled }
