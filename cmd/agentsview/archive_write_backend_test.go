package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestDaemonPushHeartbeat pins the daemon-delegated push's client-side
// output: an immediate delegation announcement, elapsed-time heartbeats
// while the blocking POST is in flight, and silence after stop.
func TestDaemonPushHeartbeat(t *testing.T) {
	orig := daemonPushHeartbeatInterval
	daemonPushHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { daemonPushHeartbeatInterval = orig })

	var out syncBuffer
	stop := startDaemonPushHeartbeatTo(&out, "PostgreSQL")
	assert.Contains(t, out.String(),
		"Pushing to PostgreSQL via the local daemon")

	require.Eventually(t, func() bool {
		return strings.Contains(out.String(),
			"still pushing to PostgreSQL via the daemon")
	}, time.Second, time.Millisecond, "heartbeat line must appear")

	stop()
	settled := out.String()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, settled, out.String(), "no output after stop")
}

// TestDaemonPushProgressSilencesHeartbeat pins that the first streamed
// progress event stops the elapsed-time heartbeat for good: once the daemon
// starts reporting real progress, heartbeat lines must not interleave with
// the in-place progress line.
func TestDaemonPushProgressSilencesHeartbeat(t *testing.T) {
	orig := daemonPushHeartbeatInterval
	daemonPushHeartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { daemonPushHeartbeatInterval = orig })

	out := captureStdout(t, func() {
		onProgress, finish := daemonPushProgress(
			"PostgreSQL", newPGPushProgressPrinter(),
		)
		onProgress(postgres.PushProgress{SessionsDone: 1, SessionsTotal: 2})
		time.Sleep(150 * time.Millisecond)
		finish()
	})

	assert.Contains(t, out, "Pushing to PostgreSQL via the local daemon")
	assert.Contains(t, out, "Pushing... 1/2 sessions")
	assert.NotContains(t, out, "still pushing",
		"heartbeat must stay silent after the first progress event")
}

// syncBuffer is a mutex-guarded bytes.Buffer: the heartbeat goroutine writes
// while the test reads.
type syncBuffer struct {
	mu  stdsync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestLocalArchiveWriteBackendPGPushStopsAfterCanceledLocalSync(t *testing.T) {
	testLocalArchivePushStopsAfterCanceledSync(t,
		func(backend *localArchiveWriteBackend, ctx context.Context) error {
			_, err := backend.PGPush(
				ctx, pgTargetSelection{}, PGPushConfig{}, nil, nil,
			)
			return err
		})
}

func TestLocalPGPushEnsuresPricingBeforeConnecting(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)
	backend.ensurePricing = func(_ context.Context, database *db.DB) error {
		require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
			ModelPattern:  "new-model",
			InputPerMTok:  2,
			OutputPerMTok: 8,
		}}))
		return nil
	}

	_, err := backend.PGPush(
		context.Background(),
		pgTargetSelection{PG: config.PGConfig{URL: unreachablePGURL}},
		PGPushConfig{}, nil, nil,
	)

	require.Error(t, err)
	rate, err := backend.database.GetModelPricing("new-model")
	require.NoError(t, err)
	require.NotNil(t, rate)
	assert.Equal(t, 8.0, rate.OutputPerMTok)
}

func TestLocalPGWatchPusherUsesBackendPricingEnsure(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)
	ensureCalls := 0
	backend.ensurePricing = func(_ context.Context, database *db.DB) error {
		require.Same(t, backend.database, database)
		ensureCalls++
		return nil
	}
	target := &fakeTarget{}
	pusher := backend.newPGPusher(
		func(context.Context) error { return nil },
		func() (pgTarget, error) { return target, nil },
	)

	require.NoError(t, pusher.push(
		context.Background(), reasonChange, false,
	))
	assert.Equal(t, 1, ensureCalls)
	assert.Equal(t, 1, target.pushes)
}

func TestLocalArchiveWriteBackendDuckDBPushStopsAfterCanceledLocalSync(t *testing.T) {
	testLocalArchivePushStopsAfterCanceledSync(t,
		func(backend *localArchiveWriteBackend, ctx context.Context) error {
			_, err := backend.DuckDBPush(
				ctx, config.DuckDBConfig{}, DuckDBPushConfig{}, nil, nil,
			)
			return err
		})
}

// TestLocalArchiveWriteBackendDuckDBPushUsesConfiguredRemoteURL verifies that
// a configured remote Quack URL now fails the push outright: push writes the
// local mirror only, so a remote target is rejected rather than attempted.
func TestLocalArchiveWriteBackendDuckDBPushUsesConfiguredRemoteURL(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)

	captureStdout(t, func() {
		_, err := backend.DuckDBPush(
			context.Background(),
			config.DuckDBConfig{
				URL:         "quack:https://duck.example.test",
				MachineName: "workstation",
			},
			DuckDBPushConfig{},
			nil,
			nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duckdb push writes the local mirror")
		assert.Contains(t, err.Error(), "quack serve")
	})
}

func TestLocalArchiveWriteBackendDuckDBPushValidatesRemoteBeforeLocalSync(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)

	var err error
	out := captureStdout(t, func() {
		_, err = backend.DuckDBPush(
			context.Background(),
			config.DuckDBConfig{
				URL:         "quack:https://duck.example.test",
				MachineName: "workstation",
			},
			DuckDBPushConfig{Full: true},
			nil,
			nil,
		)
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "duckdb push writes the local mirror")
	assert.NotContains(t, out, "Database:")
	assert.NotContains(t, out, "Opening DuckDB mirror")
}

func TestLocalArchiveWriteBackendDuckDBPushRejectsIncompleteDiscovery(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)
	original := coordinateLocalSyncRunner
	coordinateLocalSyncRunner = func(
		context.Context,
		config.Config,
		*db.DB,
		bool,
		syncpkg.ProgressFunc,
		bool,
		func() (syncpkg.RebuildOptions, syncpkg.RebuildCleanup, error),
		func(bool, bool) error,
	) (bool, syncpkg.SyncStats, error) {
		return false, syncpkg.SyncStats{Aborted: true}, nil
	}
	t.Cleanup(func() { coordinateLocalSyncRunner = original })
	mirrorPath := filepath.Join(t.TempDir(), "mirror.duckdb")

	var err error
	out := captureStdout(t, func() {
		_, err = backend.DuckDBPush(
			t.Context(),
			config.DuckDBConfig{Path: mirrorPath, MachineName: "local"},
			DuckDBPushConfig{}, nil, nil,
		)
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, "local sync discovery incomplete")
	assert.NotContains(t, out, "Opening DuckDB mirror",
		"an incomplete local archive must stop before mirror push")
}

func TestRunPGWatchStartupSyncFallsBackAfterAbortedResync(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	missingPath := filepath.Join(t.TempDir(), "missing.jsonl")
	dbtest.SeedSession(t, database, "existing", "proj",
		func(s *db.Session) {
			s.FilePath = &missingPath
		})
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{})

	didResync, err := runPGWatchStartupSync(
		context.Background(), engine, true,
	)

	require.NoError(t, err)
	assert.True(t, didResync)
	assert.False(t, engine.LastSyncStats().Aborted)
	assert.False(t, engine.LastSync().IsZero())
}

type startupDiscoveryFailureFactory struct{ err error }

func (f startupDiscoveryFailureFactory) Definition() parser.AgentDef {
	return parser.AgentDef{Type: parser.AgentCowork, FileBased: true}
}

func (f startupDiscoveryFailureFactory) Capabilities() parser.Capabilities {
	return parser.Capabilities{}
}

func (f startupDiscoveryFailureFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return &startupDiscoveryFailureProvider{
		ProviderBase: parser.ProviderBase{Def: f.Definition(), Config: cfg},
		err:          f.err,
	}
}

type startupDiscoveryFailureProvider struct {
	parser.ProviderBase
	err error
}

func (p *startupDiscoveryFailureProvider) Discover(
	context.Context,
) ([]parser.SourceRef, error) {
	return nil, p.err
}

func (p *startupDiscoveryFailureProvider) Parse(
	context.Context, parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}

func TestRunPGWatchStartupSyncReportsIncompleteDiscovery(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCowork: {root}},
		ProviderFactories: []parser.ProviderFactory{
			startupDiscoveryFailureFactory{err: errors.New("listing failed")},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	_, err := runPGWatchStartupSync(t.Context(), engine, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "discovery incomplete")
}

func TestLocalArchiveWriteBackendPGPushWatchCanceledStartupIsClean(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)

	err := backend.PGPushWatch(
		canceledContext(),
		pgTargetSelection{},
		PGPushConfig{},
		nil,
		nil,
		time.Millisecond,
		time.Millisecond,
	)

	require.NoError(t, err)
}

type pushWatchOwnerCase struct {
	name string
	run  func(context.Context, *archivePushWatchHooks) error
}

func pushWatchOwnerCases(t *testing.T) []pushWatchOwnerCase {
	t.Helper()
	return []pushWatchOwnerCase{
		{
			name: "daemon DuckDB",
			run: func(ctx context.Context, hooks *archivePushWatchHooks) error {
				return (daemonArchiveWriteBackend{watchHooks: hooks}).DuckDBPushWatch(
					ctx, config.DuckDBConfig{}, DuckDBPushConfig{}, nil, nil,
					time.Hour, time.Hour,
				)
			},
		},
		{
			name: "daemon PostgreSQL",
			run: func(ctx context.Context, hooks *archivePushWatchHooks) error {
				return (daemonArchiveWriteBackend{watchHooks: hooks}).PGPushWatch(
					ctx, pgTargetSelection{}, PGPushConfig{}, nil, nil,
					time.Hour, time.Hour,
				)
			},
		},
		{
			name: "local DuckDB",
			run: func(ctx context.Context, hooks *archivePushWatchHooks) error {
				backend := testLocalArchiveWriteBackend(t)
				backend.watchHooks = hooks
				return backend.DuckDBPushWatch(
					ctx, config.DuckDBConfig{}, DuckDBPushConfig{}, nil, nil,
					time.Hour, time.Hour,
				)
			},
		},
		{
			name: "local PostgreSQL",
			run: func(ctx context.Context, hooks *archivePushWatchHooks) error {
				backend := testLocalArchiveWriteBackend(t)
				backend.watchHooks = hooks
				return backend.PGPushWatch(
					ctx, pgTargetSelection{}, PGPushConfig{}, nil, nil,
					time.Hour, time.Hour,
				)
			},
		},
	}
}

type pushWatchAttempt struct {
	index  int
	reason pushReason
}

type pushWatchOwnerHarness struct {
	mu             stdsync.Mutex
	partial        map[int]bool
	events         []string
	attempts       chan pushWatchAttempt
	opened         chan struct{}
	callback       syncpkg.WatchCallback
	degraded       func([]string) error
	loop           *pushLoop
	fire           chan time.Time
	floor          chan time.Time
	openCount      int
	startupSyncs   int
	localPushSyncs int
	attemptCount   int
}

func newPushWatchOwnerHarness(partial ...int) *pushWatchOwnerHarness {
	h := &pushWatchOwnerHarness{
		partial:  make(map[int]bool, len(partial)),
		attempts: make(chan pushWatchAttempt, 8),
		opened:   make(chan struct{}, 2),
		fire:     make(chan time.Time, 2),
		floor:    make(chan time.Time, 2),
	}
	for _, index := range partial {
		h.partial[index] = true
	}
	return h
}

func (h *pushWatchOwnerHarness) hooks() *archivePushWatchHooks {
	return &archivePushWatchHooks{
		startWatcher: func(
			_ config.Config, _ *syncpkg.Engine, callback syncpkg.WatchCallback,
			options syncpkg.WatcherOptions,
		) (func(), func(), []string) {
			h.record("collect")
			h.mu.Lock()
			h.callback = callback
			h.degraded = options.OnCoverageDegraded
			h.mu.Unlock()
			return func() {}, func() {
				h.mu.Lock()
				h.openCount++
				h.events = append(h.events, "open")
				h.mu.Unlock()
				h.opened <- struct{}{}
			}, nil
		},
		newLoop: func(
			label string, _ time.Duration, _ time.Duration,
			push func(context.Context, pushReason) error,
		) (*pushLoop, func()) {
			h.loop = &pushLoop{
				debounce: time.Hour,
				dirty:    make(chan struct{}, 1),
				floor:    h.floor,
				after:    func(time.Duration) <-chan time.Time { return h.fire },
				push:     push,
				label:    label,
			}
			return h.loop, func() {}
		},
		duckDBPush: func(
			_ context.Context, reason pushReason, _ bool,
		) (duckdbsync.PushResult, error) {
			attempt, partial := h.nextAttempt(reason)
			if partial {
				return duckdbsync.PushResult{Errors: 1}, nil
			}
			return duckdbsync.PushResult{SessionsPushed: attempt}, nil
		},
		pgPush: func(
			_ context.Context, reason pushReason, _ bool,
		) (postgres.PushResult, error) {
			attempt, partial := h.nextAttempt(reason)
			if partial {
				return postgres.PushResult{Errors: 1}, nil
			}
			return postgres.PushResult{SessionsPushed: attempt}, nil
		},
		pgStartupSync: func(
			context.Context, *syncpkg.Engine, bool,
		) (bool, error) {
			h.mu.Lock()
			h.startupSyncs++
			h.events = append(h.events, "startup-sync")
			h.mu.Unlock()
			return false, nil
		},
		newPGPusher: func(*syncpkg.Engine) *pgPusher {
			target := &pushWatchPGTarget{harness: h}
			return &pgPusher{
				localSync: func(context.Context) error {
					h.mu.Lock()
					h.localPushSyncs++
					h.events = append(h.events, "local-sync")
					h.mu.Unlock()
					return nil
				},
				connect: func() (pgTarget, error) { return target, nil },
			}
		},
	}
}

func (h *pushWatchOwnerHarness) record(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
}

func (h *pushWatchOwnerHarness) nextAttempt(reason pushReason) (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nextAttemptLocked(reason)
}

func (h *pushWatchOwnerHarness) nextPGAttempt() (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nextAttemptLocked("")
}

func (h *pushWatchOwnerHarness) nextAttemptLocked(
	reason pushReason,
) (int, bool) {
	h.attemptCount++
	index := h.attemptCount
	h.events = append(h.events, "push")
	partial := h.partial[index]
	h.attempts <- pushWatchAttempt{index: index, reason: reason}
	return index, partial
}

func (h *pushWatchOwnerHarness) pending() (bool, int) {
	h.loop.pendingMu.Lock()
	defer h.loop.pendingMu.Unlock()
	return h.loop.pending, len(h.loop.waiters)
}

func (h *pushWatchOwnerHarness) snapshot() (
	[]string, int, int, int,
) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...), h.openCount,
		h.startupSyncs, h.localPushSyncs
}

func (h *pushWatchOwnerHarness) watcherCallback() syncpkg.WatchCallback {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.callback
}

func (h *pushWatchOwnerHarness) degradationCallback() func([]string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.degraded
}

type pushWatchPGTarget struct {
	harness *pushWatchOwnerHarness
}

func (*pushWatchPGTarget) EnsureSchema(context.Context) error { return nil }
func (t *pushWatchPGTarget) Push(
	_ context.Context, _ bool, _ func(postgres.PushProgress),
) (postgres.PushResult, error) {
	attempt, partial := t.harness.nextPGAttempt()
	if partial {
		return postgres.PushResult{Errors: 1}, nil
	}
	return postgres.PushResult{SessionsPushed: attempt}, nil
}
func (*pushWatchPGTarget) Close() error { return nil }

func TestPushWatchProductionOwnersRetainPartialStartupUntilPeriodicSuccess(
	t *testing.T,
) {
	for _, owner := range pushWatchOwnerCases(t) {
		t.Run(owner.name, func(t *testing.T) {
			h := newPushWatchOwnerHarness(1)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() { done <- owner.run(ctx, h.hooks()) }()

			first := receiveArchiveTest(t, h.attempts)
			require.Equal(t, 1, first.index)
			if owner.name != "local PostgreSQL" {
				assert.Equal(t, reasonStartup, first.reason)
			}
			require.Eventually(t, func() bool {
				pending, waiters := h.pending()
				return pending && waiters == 1
			}, time.Second, time.Millisecond,
				"partial startup must retain one dirty generation and waiter")
			select {
			case <-h.opened:
				require.Fail(t, "partial startup opened watcher dispatch")
			default:
			}

			h.floor <- time.Now()
			second := receiveArchiveTest(t, h.attempts)
			require.Equal(t, 2, second.index)
			if owner.name != "local PostgreSQL" {
				assert.Equal(t, reasonInterval, second.reason)
			}
			receiveArchiveTest(t, h.opened)
			require.Eventually(t, func() bool {
				pending, waiters := h.pending()
				return !pending && waiters == 0
			}, time.Second, time.Millisecond,
				"complete periodic push must drain startup work")

			events, opens, startupSyncs, localSyncs := h.snapshot()
			require.NotEmpty(t, events)
			assert.Equal(t, "collect", events[0],
				"watcher collection must precede startup work")
			assert.Equal(t, 1, opens)
			if owner.name == "local PostgreSQL" {
				assert.Equal(t, 1, startupSyncs)
				assert.GreaterOrEqual(t, localSyncs, 2,
					"initial and retry pushes each run local sync")
				assert.Less(t, indexOfEvent(events, "startup-sync"),
					indexOfEvent(events, "push"))
				assert.Less(t, indexOfEvent(events, "local-sync"),
					indexOfEvent(events, "push"))
			}

			cancel()
			require.NoError(t, receiveArchiveTest(t, done))
		})
	}
}

func TestPushWatchProductionOwnersRetryPartialAuthoritativeBatch(
	t *testing.T,
) {
	for _, owner := range pushWatchOwnerCases(t) {
		t.Run(owner.name, func(t *testing.T) {
			h := newPushWatchOwnerHarness(2)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() { done <- owner.run(ctx, h.hooks()) }()

			first := receiveArchiveTest(t, h.attempts)
			require.Equal(t, 1, first.index)
			if owner.name != "local PostgreSQL" {
				assert.Equal(t, reasonStartup, first.reason)
			}
			receiveArchiveTest(t, h.opened)
			callback := h.watcherCallback()
			require.NotNil(t, callback)
			callbackDone := make(chan error, 1)
			go func() {
				callbackDone <- callback(ctx, syncpkg.WatchBatch{FullSync: true})
			}()
			require.Eventually(t, func() bool {
				pending, waiters := h.pending()
				return pending && waiters == 1
			}, time.Second, time.Millisecond)

			h.floor <- time.Now()
			second := receiveArchiveTest(t, h.attempts)
			require.Equal(t, 2, second.index)
			if owner.name != "local PostgreSQL" {
				assert.Equal(t, reasonInterval, second.reason)
			}
			select {
			case err := <-callbackDone:
				require.Fail(t, "partial runtime push acknowledged watcher batch", "%v", err)
			case <-time.After(50 * time.Millisecond):
			}
			require.Eventually(t, func() bool {
				pending, waiters := h.pending()
				return pending && waiters == 1
			}, time.Second, time.Millisecond)

			h.floor <- time.Now()
			third := receiveArchiveTest(t, h.attempts)
			require.Equal(t, 3, third.index)
			if owner.name != "local PostgreSQL" {
				assert.Equal(t, reasonInterval, third.reason)
			}
			require.NoError(t, receiveArchiveTest(t, callbackDone))
			require.Eventually(t, func() bool {
				pending, waiters := h.pending()
				return !pending && waiters == 0
			}, time.Second, time.Millisecond)
			_, opens, _, _ := h.snapshot()
			assert.Equal(t, 1, opens, "runtime retries must not reopen dispatch")

			cancel()
			require.NoError(t, receiveArchiveTest(t, done))
		})
	}
}

func TestPushWatchProductionOwnersFallbackUsesActiveIntervalFloor(t *testing.T) {
	for _, owner := range pushWatchOwnerCases(t) {
		t.Run(owner.name, func(t *testing.T) {
			h := newPushWatchOwnerHarness()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- owner.run(ctx, h.hooks()) }()

			receiveArchiveTest(t, h.attempts)
			receiveArchiveTest(t, h.opened)
			degraded := h.degradationCallback()
			require.NotNil(t, degraded, "watcher collection receives a degradation owner")
			require.NoError(t, degraded([]string{"/root-a", "/root-a", "/root-b"}))
			require.Eventually(t, func() bool {
				pending, waiters := h.pending()
				return pending && waiters == 0
			}, time.Second, time.Millisecond)

			h.floor <- time.Now()
			attempt := receiveArchiveTest(t, h.attempts)
			if owner.name != "local PostgreSQL" {
				assert.Equal(t, reasonInterval, attempt.reason)
			}
			cancel()
			require.NoError(t, receiveArchiveTest(t, done))
		})
	}
}

func indexOfEvent(events []string, want string) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return -1
}

func receiveArchiveTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for archive watch result")
		var zero T
		return zero
	}
}

func TestPushWatchCollectingCallbackAcknowledgesOnlyReconciliationBatches(t *testing.T) {
	loop, _, _ := newTestLoop(func(context.Context, pushReason) error {
		return nil
	})

	require.NoError(t, notifyPushForWatchBatch(
		t.Context(), loop, syncpkg.WatchBatch{Paths: []string{"session.jsonl"}},
	), "ordinary changes only enqueue non-blocking work")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := notifyPushForWatchBatch(ctx, loop, syncpkg.WatchBatch{FullSync: true})
	require.ErrorIs(t, err, context.Canceled,
		"authoritative batches wait with the watcher worker context")
}

func TestResolveArchiveWriteBackendCopiesNoSyncRuntime(t *testing.T) {
	dataDir := t.TempDir()
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dataDir, host, port, "test", false, false, true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dataDir) })

	backend, cleanup, err := resolveArchiveWriteBackend(
		context.Background(), config.Config{DataDir: dataDir},
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	daemonBackend, ok := backend.(daemonArchiveWriteBackend)
	require.True(t, ok)
	assert.True(t, daemonBackend.appCfg.NoSync)
}

// canceledContext returns a context that has already been canceled.
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// testLocalArchivePushStopsAfterCanceledSync asserts that a local push aborts
// with context.Canceled when its context is already canceled. push runs the
// backend-specific push call and returns its error.
func testLocalArchivePushStopsAfterCanceledSync(
	t *testing.T,
	push func(*localArchiveWriteBackend, context.Context) error,
) {
	t.Helper()
	backend := testLocalArchiveWriteBackend(t)

	var err error
	captureStdout(t, func() {
		err = push(backend, canceledContext())
	})

	require.ErrorIs(t, err, context.Canceled)
}

type noopPGTarget struct{}

func (noopPGTarget) EnsureSchema(context.Context) error { return nil }
func (noopPGTarget) Push(
	context.Context, bool, func(postgres.PushProgress),
) (postgres.PushResult, error) {
	return postgres.PushResult{}, nil
}
func (noopPGTarget) Close() error { return nil }

// The watcher's full recovery and rename promotion defer unavailable scopes
// to their polling probes, and pg watch's interval push is a plain SyncAll
// that never tombstones missed deletions. Local pg watch must therefore own
// those deferred scopes with a probe-gated authoritative poller: a root the
// watcher cannot cover (here a symlinked recursive root, the same obligation
// machinery that owns roots missing at startup on portable backends)
// registers a polling obligation, and the poller reconciles its scope
// authoritatively on ticks — with no watcher event and no floor push
// involved.
func TestLocalPGPushWatchGivesDeferredScopesAPollingOwner(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	target := t.TempDir()
	codexRoot := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.Symlink(target, codexRoot))

	backend := &localArchiveWriteBackend{
		appCfg: config.Config{
			DataDir:          dataDir,
			DBPath:           dbPath,
			LocalMachineName: "local",
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentCodex: {codexRoot},
			},
		},
		database: database,
	}

	ticks := make(chan time.Time, 1)
	owned := make(chan []string, 8)
	fire := make(chan time.Time)
	floor := make(chan time.Time)
	backend.watchHooks = &archivePushWatchHooks{
		newLoop: func(
			label string, _, _ time.Duration,
			push func(context.Context, pushReason) error,
		) (*pushLoop, func()) {
			return &pushLoop{
				debounce: time.Hour,
				dirty:    make(chan struct{}, 1),
				floor:    floor,
				after:    func(time.Duration) <-chan time.Time { return fire },
				push:     push,
				label:    label,
			}, func() {}
		},
		pgStartupSync: func(context.Context, *syncpkg.Engine, bool) (bool, error) {
			return false, nil
		},
		newPGPusher: func(*syncpkg.Engine) *pgPusher {
			return &pgPusher{
				localSync: func(context.Context) error { return nil },
				connect:   func() (pgTarget, error) { return noopPGTarget{}, nil },
			}
		},
		newUnwatchedPoller: func(
			ctx context.Context, engine unwatchedPollSyncer,
		) unwatchedRootPoller {
			return newUnwatchedPollCoordinatorWithTicks(
				ctx, engine, ticks, func() {}, func(work func()) { work() },
				func(roots []string) {
					owned <- append([]string(nil), roots...)
				},
			)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- backend.PGPushWatch(
			ctx, pgTargetSelection{}, PGPushConfig{}, nil, nil,
			time.Hour, time.Hour,
		)
	}()

	select {
	case roots := <-owned:
		assert.Contains(t, roots, codexRoot,
			"the unwatchable root's polling obligation must reach the pg watch poller")
	case err := <-done:
		t.Fatalf("pg watch exited before registering obligations: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no polling obligation was registered for the unwatchable root")
	}

	// A session lands under the unwatched scope; only the poller's
	// authoritative tick can bring it into the archive.
	uuid := "d4e5f6a7-4444-4555-8666-777788889999"
	day := filepath.Join(codexRoot, "2026", "05", "04")
	require.NoError(t, os.MkdirAll(day, 0o755))
	content := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			"2026-05-04T14:00:00Z", uuid, "/home/user/code/api",
			"codex_cli_rs",
		).
		AddCodexMessage("2026-05-04T14:00:01Z", "user", "hello").
		String()
	require.NoError(t, os.WriteFile(
		filepath.Join(day, "rollout-2026-05-04T14-31-58-"+uuid+".jsonl"),
		[]byte(content), 0o644,
	))

	require.Eventually(t, func() bool {
		select {
		case ticks <- time.Now():
		default:
		}
		session, err := database.GetSession(context.Background(), "codex:"+uuid)
		return err == nil && session != nil
	}, 10*time.Second, 20*time.Millisecond,
		"the returned root must be reconciled by the poller without watcher events or floor pushes")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("pg watch did not shut down")
	}
}

func testLocalArchiveWriteBackend(t *testing.T) *localArchiveWriteBackend {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	database := dbtest.OpenTestDBAt(t, dbPath)

	return &localArchiveWriteBackend{
		appCfg: config.Config{
			DataDir: dataDir,
			DBPath:  dbPath,
		},
		database: database,
	}
}
