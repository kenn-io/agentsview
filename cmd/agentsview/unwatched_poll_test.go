package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

type recordingUnwatchedPollSyncer struct {
	mu           sync.Mutex
	calls        [][]string
	full         []bool
	wake         chan struct{}
	reconcileErr error
}

type blockingUnwatchedPollSyncer struct {
	mu        sync.Mutex
	started   chan []string
	release   chan struct{}
	calls     [][]string
	active    int
	maxActive int
}

type cancelBlockingUnwatchedPollSyncer struct {
	mu       sync.Mutex
	started  chan struct{}
	canceled chan struct{}
	calls    int
}

func (s *cancelBlockingUnwatchedPollSyncer) ReconcileWatchRoots(
	ctx context.Context, _ []string, _ bool,
) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	s.started <- struct{}{}
	<-ctx.Done()
	s.canceled <- struct{}{}
	return ctx.Err()
}

func (s *cancelBlockingUnwatchedPollSyncer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *blockingUnwatchedPollSyncer) ReconcileWatchRoots(
	_ context.Context, roots []string, _ bool,
) error {
	owned := append([]string(nil), roots...)
	s.mu.Lock()
	s.calls = append(s.calls, owned)
	s.active++
	s.maxActive = max(s.maxActive, s.active)
	s.mu.Unlock()
	s.started <- owned
	<-s.release
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return nil
}

func (s *blockingUnwatchedPollSyncer) snapshot() ([][]string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	calls := make([][]string, len(s.calls))
	for i := range s.calls {
		calls[i] = append([]string(nil), s.calls[i]...)
	}
	return calls, s.maxActive
}

func (s *recordingUnwatchedPollSyncer) ReconcileWatchRoots(
	_ context.Context, roots []string, full bool,
) error {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string(nil), roots...))
	s.full = append(s.full, full)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return s.reconcileErr
}

func (s *recordingUnwatchedPollSyncer) snapshot() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([][]string, len(s.calls))
	for i := range s.calls {
		result[i] = append([]string(nil), s.calls[i]...)
	}
	return result
}

func TestUnwatchedPollConcurrentAddDeduplicatesUpdatedRootSet(t *testing.T) {
	ticks := make(chan time.Time)
	syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 4)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, ticks, func() {}, func(run func()) { run() }, nil,
	)
	t.Cleanup(coordinator.Stop)
	parent := t.TempDir()
	rootA := requireExistingPollRoot(t, parent, "root-a")
	rootB := requireExistingPollRoot(t, parent, "root-b")
	rootC := requireExistingPollRoot(t, parent, "root-c")

	additions := [][]string{
		{rootB, rootA},
		{rootA, rootC},
		{rootC, rootB},
	}
	var wg sync.WaitGroup
	addErrors := make(chan error, len(additions))
	for i, roots := range additions {
		wg.Go(func() {
			addErrors <- coordinator.AddObligation(pollingObligation{
				Key: fmt.Sprintf("direct-%d", i), Roots: roots,
			})
		})
	}
	wg.Wait()
	close(addErrors)
	for err := range addErrors {
		require.NoError(t, err)
	}

	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)
	assert.Equal(t, [][]string{{rootA, rootB, rootC}}, syncer.snapshot())
}

func TestUnwatchedPollTickUsesRootsAddedAfterStart(t *testing.T) {
	ticks := make(chan time.Time, 1)
	syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 2)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, ticks, func() {}, func(run func()) { run() }, nil,
	)
	t.Cleanup(coordinator.Stop)
	parent := t.TempDir()
	initial := requireExistingPollRoot(t, parent, "initial")
	runtime := requireExistingPollRoot(t, parent, "runtime")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "initial", Roots: []string{initial},
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "runtime", Roots: []string{runtime},
	}))

	ticks <- time.Now()
	requirePollWithin(t, syncer.wake, time.Second)

	assert.Equal(t, [][]string{{initial, runtime}}, syncer.snapshot())
	assert.Equal(t, []bool{false}, syncer.full,
		"unwatched polling must reconcile the owned scopes authoritatively")
}

func TestUnwatchedPollSkipsAbsentObligatedRootUntilItReturns(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "provider")
	require.NoError(t, os.Mkdir(root, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "session.jsonl"), []byte("session\n"), 0o600,
	))

	syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 3)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, make(chan time.Time), func() {},
		func(run func()) { run() }, nil,
	)
	t.Cleanup(coordinator.Stop)
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "provider-root", Roots: []string{root},
	}))

	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)
	assert.Equal(t, [][]string{{root}}, syncer.snapshot())

	require.NoError(t, os.RemoveAll(root))
	coordinator.requestPoll()
	assert.Never(t, func() bool { return len(syncer.snapshot()) > 1 },
		100*time.Millisecond, 10*time.Millisecond,
		"an absent root must not become an authoritative empty scope")

	require.NoError(t, os.Mkdir(root, 0o755))
	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)
	assert.Equal(t, [][]string{{root}, {root}}, syncer.snapshot(),
		"the polling obligation must remain active for a returning root")
}

// TestUnwatchedPollDefersScopesWhileProbePathMissing is the nested-root
// regression (Gemini's <root>/tmp): the obligation's reconciliation scope is
// the configured <root>, but its physical watcher path is <root>/tmp. While
// the physical path is missing, polling must defer the scope entirely instead
// of authoritatively reconciling the still-present <root>, which would
// tombstone every session under the vanished subtree.
func TestUnwatchedPollDefersScopesWhileProbePathMissing(t *testing.T) {
	configured := t.TempDir()
	physical := filepath.Join(configured, "tmp")
	require.NoError(t, os.Mkdir(physical, 0o755))

	syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 3)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, make(chan time.Time), func() {},
		func(run func()) { run() }, nil,
	)
	t.Cleanup(coordinator.Stop)
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: physical, Roots: []string{configured}, Probe: physical,
	}))

	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)
	assert.Equal(t, [][]string{{configured}}, syncer.snapshot(),
		"an available probe reconciles the configured scope")

	require.NoError(t, os.RemoveAll(physical))
	coordinator.requestPoll()
	assert.Never(t, func() bool { return len(syncer.snapshot()) > 1 },
		100*time.Millisecond, 10*time.Millisecond,
		"a missing physical watcher path must defer its reconciliation "+
			"scopes even though the configured root still exists")

	require.NoError(t, os.Mkdir(physical, 0o755))
	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)
	assert.Equal(t, [][]string{{configured}, {configured}}, syncer.snapshot(),
		"the deferred scope must resume once the physical path returns")
}

// TestUnwatchedPollDefersSharedScopeWhileAnyProbeMissing pins the shared-scope
// gating: Gemini's shallow <root> metadata plan and recursive <root>/tmp plan
// both reconcile the configured <root>. While <root>/tmp is missing, the
// present shallow plan must not make <root> pollable, or authoritative
// reconciliation would tombstone every session under the vanished subtree.
func TestUnwatchedPollDefersSharedScopeWhileAnyProbeMissing(t *testing.T) {
	configured := t.TempDir()
	sessions := filepath.Join(configured, "tmp")
	require.NoError(t, os.Mkdir(sessions, 0o755))

	syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 3)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, make(chan time.Time), func() {},
		func(run func()) { run() }, nil,
	)
	t.Cleanup(coordinator.Stop)
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: configured, Roots: []string{configured}, Probe: configured,
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: sessions, Roots: []string{configured}, Probe: sessions,
	}))

	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)
	assert.Equal(t, [][]string{{configured}}, syncer.snapshot(),
		"with every probe available the shared scope reconciles")

	require.NoError(t, os.RemoveAll(sessions))
	coordinator.requestPoll()
	assert.Never(t, func() bool { return len(syncer.snapshot()) > 1 },
		100*time.Millisecond, 10*time.Millisecond,
		"a missing session subtree must defer the shared scope even though "+
			"the metadata plan's probe still exists")

	require.NoError(t, os.Mkdir(sessions, 0o755))
	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)
	assert.Equal(t, [][]string{{configured}, {configured}}, syncer.snapshot(),
		"the shared scope must resume once every probe returns")
}

// TestAvailableUnwatchedPollRootsDefersRootsOverlappingBlockedScopes pins the
// cross-obligation analogue of overlapsDeferredScope: a missing probe blocks
// its own roots, but ReconcileWatchRoots expands every requested root to the
// configured dirs above and below it, so a still-available ancestor or
// descendant root from another obligation would pull the deferred scope back
// into an authoritative pass and tombstone its sessions.
func TestAvailableUnwatchedPollRootsDefersRootsOverlappingBlockedScopes(t *testing.T) {
	base := t.TempDir()
	nested := requireExistingPollRoot(t, base, "nested")
	missingProbe := filepath.Join(nested, "missing-probe")
	unrelated := requireExistingPollRoot(t, t.TempDir(), "other")

	tests := []struct {
		name        string
		obligations []pollingObligation
		want        []string
	}{
		{
			name: "blocked descendant defers available ancestor",
			obligations: []pollingObligation{
				{Key: "base", Roots: []string{base, unrelated}},
				{Key: "nested", Roots: []string{nested}, Probe: missingProbe},
			},
			want: []string{unrelated},
		},
		{
			name: "blocked ancestor defers available descendant",
			obligations: []pollingObligation{
				{Key: "nested", Roots: []string{nested, unrelated}},
				{Key: "base", Roots: []string{base}, Probe: filepath.Join(base, "gone")},
			},
			want: []string{unrelated},
		},
		{
			name: "available probes keep overlapping roots pollable",
			obligations: []pollingObligation{
				{Key: "base", Roots: []string{base}},
				{Key: "nested", Roots: []string{nested}, Probe: nested},
			},
			want: []string{base, nested},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, availableUnwatchedPollRoots(tc.obligations))
		})
	}
}

// TestUnwatchedPollPreservesSessionsUnderBlockedOverlappingScope drives the
// real engine: agent A's configured dir is an ancestor of agent B's, B's probe
// is missing, and A stays pollable. Polling A's root must not expand into B's
// configured scope and tombstone B's baselined session as an empty discovery.
func TestUnwatchedPollPreservesSessionsUnderBlockedOverlappingScope(t *testing.T) {
	base := t.TempDir()
	nested := requireExistingPollRoot(t, base, "nested")
	sourcePath := filepath.Join(nested, "project", "archived-session.jsonl")

	database := dbtest.OpenTestDB(t)
	const sessionID = "claude:archived"
	dbtest.SeedSession(t, database, sessionID, "project",
		func(session *db.Session) {
			session.Agent = string(parser.AgentClaude)
			session.FilePath = &sourcePath
		})
	require.NoError(t,
		database.SetSessionDataVersion(sessionID, db.CurrentDataVersion()))
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "local", []db.SessionSourcePath{{
			Agent: string(parser.AgentClaude), FilePath: sourcePath,
		}},
	))
	engine := agentsync.NewEngine(database, agentsync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentOpenHands: {base},
			parser.AgentClaude:    {nested},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)

	obligations := []pollingObligation{
		{Key: "persistent:" + base, Roots: []string{base}, Probe: base},
		{Key: "nested-gate", Roots: []string{nested},
			Probe: filepath.Join(nested, "missing-subtree")},
	}
	roots := availableUnwatchedPollRoots(obligations)
	pollUnwatchedRootsOnce(t.Context(), engine, roots)

	assert.Empty(t, roots,
		"an ancestor overlapping a blocked scope must not stay pollable")
	preserved, err := database.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.NotNil(t, preserved,
		"polling must not tombstone sessions under the deferred nested scope")
}

func TestUnwatchedPollObligationUpdatesRemainResponsiveDuringReconciliation(
	t *testing.T,
) {
	ticks := make(chan time.Time)
	syncer := &blockingUnwatchedPollSyncer{
		started: make(chan []string, 4),
		release: make(chan struct{}),
	}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		context.Background(), syncer, ticks, func() {}, func(run func()) { run() }, nil,
	)
	t.Cleanup(func() {
		select {
		case <-syncer.release:
		default:
			close(syncer.release)
		}
		coordinator.Stop()
	})
	parent := t.TempDir()
	initial := requireExistingPollRoot(t, parent, "initial")
	replacement := requireExistingPollRoot(t, parent, "replacement")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "initial", Roots: []string{initial},
	}))

	coordinator.requestPoll()
	assert.Equal(t, []string{initial},
		requireReceivePollRoots(t, syncer.started, time.Second))

	addResult := make(chan error, 1)
	go func() {
		addResult <- coordinator.AddObligation(pollingObligation{
			Key: "replacement", Roots: []string{replacement},
		})
	}()
	require.NoError(t, requireReceivePollResult(t, addResult, time.Second),
		"watcher polling callbacks must not wait for reconciliation")
	removeResult := make(chan error, 1)
	go func() {
		removeResult <- coordinator.RemoveObligation("initial")
	}()
	require.NoError(t, requireReceivePollResult(t, removeResult, time.Second),
		"watcher polling removals must not wait for reconciliation")
	coordinator.requestPoll()
	coordinator.requestPoll()

	close(syncer.release)
	assert.Equal(t, []string{replacement},
		requireReceivePollRoots(t, syncer.started, time.Second))
	calls, maxActive := syncer.snapshot()
	assert.Equal(t, [][]string{{initial}, {replacement}}, calls)
	assert.Equal(t, 1, maxActive, "poll reconciliations must remain serialized")
}

func TestUnwatchedPollStopCancelsAndJoinsActiveReconciliation(t *testing.T) {
	parentCtx, cancelParent := context.WithCancel(context.Background())
	syncer := &cancelBlockingUnwatchedPollSyncer{
		started:  make(chan struct{}, 2),
		canceled: make(chan struct{}, 2),
	}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		parentCtx, syncer, make(chan time.Time), func() {},
		func(run func()) { run() }, nil,
	)
	t.Cleanup(func() {
		cancelParent()
		coordinator.Stop()
	})
	owned := requireExistingPollRoot(t, t.TempDir(), "owned")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "owned", Roots: []string{owned},
	}))
	coordinator.requestPoll()
	requirePollWithin(t, syncer.started, time.Second)
	coordinator.requestPoll()

	stopDone := make(chan struct{})
	go func() {
		coordinator.Stop()
		close(stopDone)
	}()
	requirePollWithin(t, stopDone, time.Second)
	requirePollWithin(t, syncer.canceled, time.Second)
	assert.Equal(t, 1, syncer.callCount(),
		"shutdown must discard the wake queued during reconciliation")

	coordinator.requestPoll()
	assert.Never(t, func() bool { return syncer.callCount() > 1 },
		100*time.Millisecond, 10*time.Millisecond,
		"shutdown must not start another queued reconciliation")
}

func TestUnwatchedPollParentCancellationCancelsJoinsAndRejectsUpdates(
	t *testing.T,
) {
	parentCtx, cancelParent := context.WithCancel(context.Background())
	syncer := &cancelBlockingUnwatchedPollSyncer{
		started:  make(chan struct{}, 1),
		canceled: make(chan struct{}, 1),
	}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		parentCtx, syncer, make(chan time.Time), func() {},
		func(run func()) { run() }, nil,
	)
	t.Cleanup(func() {
		cancelParent()
		coordinator.Stop()
	})
	owned := requireExistingPollRoot(t, t.TempDir(), "owned")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "owned", Roots: []string{owned},
	}))
	coordinator.requestPoll()
	requirePollWithin(t, syncer.started, time.Second)

	cancelParent()
	requirePollWithin(t, syncer.canceled, time.Second)
	select {
	case <-coordinator.done:
	case <-time.After(time.Second):
		require.FailNow(t, "parent cancellation did not join the poll worker")
	}

	lateUpdate := make(chan error, 1)
	go func() {
		lateUpdate <- coordinator.AddObligation(pollingObligation{
			Key: "late", Roots: []string{"/late"},
		})
	}()
	assert.ErrorIs(t, requireReceivePollResult(t, lateUpdate, time.Second),
		errUnwatchedPollStopped)
	assert.Equal(t, 1, syncer.callCount())
}

func TestUnwatchedPollRemoveRootsStopsReconciliationAfterNativeRecovery(t *testing.T) {
	ticks := make(chan time.Time, 1)
	syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 2)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, ticks, func() {}, func(run func()) { run() }, nil,
	)
	t.Cleanup(coordinator.Stop)
	parent := t.TempDir()
	recovered := requireExistingPollRoot(t, parent, "recovered")
	stillUnwatched := requireExistingPollRoot(t, parent, "still-unwatched")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "recovered-watch", Roots: []string{recovered},
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "still-unwatched", Roots: []string{stillUnwatched},
	}))
	require.NoError(t, coordinator.RemoveObligation("recovered-watch"))

	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)

	assert.Equal(t, [][]string{{stillUnwatched}}, syncer.snapshot())
}

func TestUnwatchedPollRemovingOneOverlappingObligationKeepsSharedRoot(t *testing.T) {
	ticks := make(chan time.Time)
	syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 2)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, ticks, func() {}, func(run func()) { run() }, nil,
	)
	t.Cleanup(coordinator.Stop)
	parent := t.TempDir()
	shared := requireExistingPollRoot(t, parent, "shared")
	persistentOnly := requireExistingPollRoot(t, parent, "persistent-only")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "pending", Roots: []string{shared},
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "persistent", Roots: []string{shared, persistentOnly},
	}))
	require.NoError(t, coordinator.RemoveObligation("pending"))

	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)

	assert.Equal(t,
		[][]string{{persistentOnly, shared}}, syncer.snapshot())
}

func TestUnwatchedPollEmptyObligationNeverExpandsToFullReconciliation(t *testing.T) {
	ticks := make(chan time.Time)
	syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 1)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, ticks, func() {}, func(run func()) { run() }, nil,
	)
	t.Cleanup(coordinator.Stop)
	require.NoError(t, coordinator.AddObligation(pollingObligation{Key: "empty"}))

	coordinator.requestPoll()

	assert.Never(t, func() bool { return len(syncer.snapshot()) > 0 },
		100*time.Millisecond, 10*time.Millisecond)
}

func TestUnwatchedPollStopIsConcurrentAndRejectsLaterRoots(t *testing.T) {
	ticks := make(chan time.Time)
	syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 1)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		context.Background(), syncer, ticks, func() {}, func(run func()) { run() }, nil,
	)
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "owned", Roots: []string{"/owned"},
	}))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			coordinator.Stop()
		})
	}
	wg.Wait()

	assert.ErrorIs(t, coordinator.AddObligation(pollingObligation{
		Key: "late", Roots: []string{"/late"},
	}), errUnwatchedPollStopped)
	coordinator.requestPoll()
	assert.Empty(t, syncer.snapshot())
}

func TestUnwatchedPollAddObligationRacingStopReturnsOwnershipOrStopped(t *testing.T) {
	const attempts = 64
	for i := range attempts {
		ticks := make(chan time.Time)
		syncer := &recordingUnwatchedPollSyncer{wake: make(chan struct{}, 1)}
		ownedSnapshots := make(chan []string, 1)
		coordinator := newUnwatchedPollCoordinatorWithTicks(
			context.Background(), syncer, ticks, func() {}, func(run func()) { run() },
			func(roots []string) {
				ownedSnapshots <- append([]string(nil), roots...)
			},
		)
		start := make(chan struct{})
		addResult := make(chan error, 1)
		stopDone := make(chan struct{})
		root := fmt.Sprintf("/race-root-%d", i)
		go func() {
			<-start
			addResult <- coordinator.AddObligation(pollingObligation{
				Key: root, Roots: []string{root},
			})
		}()
		go func() {
			<-start
			coordinator.Stop()
			close(stopDone)
		}()

		close(start)
		err := requireReceivePollResult(t, addResult, time.Second)
		requirePollWithin(t, stopDone, time.Second)
		if err != nil {
			assert.ErrorIs(t, err, errUnwatchedPollStopped)
		} else {
			owned := requireReceivePollRoots(t, ownedSnapshots, time.Second)
			assert.Contains(t, owned, root)
		}
		assert.ErrorIs(t,
			coordinator.AddObligation(pollingObligation{
				Key:   fmt.Sprintf("/late-root-%d", i),
				Roots: []string{fmt.Sprintf("/late-root-%d", i)},
			}),
			errUnwatchedPollStopped,
		)
	}
}

func requireReceivePollRoots(
	t *testing.T,
	results <-chan []string,
	timeout time.Duration,
) []string {
	t.Helper()
	select {
	case roots := <-results:
		return roots
	case <-time.After(timeout):
		require.FailNow(t, "poll coordinator ownership did not arrive before timeout")
		return nil
	}
}

func requireExistingPollRoot(t *testing.T, parent, name string) string {
	t.Helper()
	root := filepath.Join(parent, name)
	require.NoError(t, os.Mkdir(root, 0o755))
	return root
}

func requireReceivePollResult(
	t *testing.T,
	results <-chan error,
	timeout time.Duration,
) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(timeout):
		require.FailNow(t, "poll coordinator result did not arrive before timeout")
		return nil
	}
}

func requirePollWithin(t *testing.T, ch <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		require.FailNow(t, "poll did not run before timeout")
	}
}
