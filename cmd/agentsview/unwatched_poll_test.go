package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

type failingBoundedCoverageAdmission struct{}

func (failingBoundedCoverageAdmission) BoundedCoverageBindings(
	context.Context, []agentsync.BoundedCoverageRoot,
) ([]agentsync.BoundedCoverageBinding, error) {
	return nil, nil
}

func (failingBoundedCoverageAdmission) BoundedCoverageBindingsForPaths(
	context.Context, []string,
) ([]agentsync.BoundedCoverageBinding, []string, error) {
	return nil, nil, nil
}

func (failingBoundedCoverageAdmission) DrainBoundedCoverage(
	context.Context, agentsync.BoundedCoverageBinding, parser.OpenCodeCoverageCheckpoint,
) (parser.OpenCodeFeedResult, []parser.SourceRef, error) {
	return parser.OpenCodeFeedResult{}, nil, nil
}

func (failingBoundedCoverageAdmission) ApplyBoundedCoverageSources(
	context.Context, []parser.SourceRef,
) (agentsync.SyncStats, error) {
	return agentsync.SyncStats{}, nil
}

func (failingBoundedCoverageAdmission) InitializeBoundedCoverage(
	context.Context, agentsync.BoundedCoverageBinding,
) (parser.OpenCodeCoverageCheckpoint, error) {
	return parser.OpenCodeCoverageCheckpoint{}, errors.New("admission failed")
}

func TestFailedBoundedCoverageAdmissionRestoresFrozenState(t *testing.T) {
	oldPath := filepath.Join(t.TempDir(), "old.db")
	newPath := filepath.Join(t.TempDir(), "new.db")
	require.NoError(t, os.WriteFile(oldPath, []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(newPath, []byte("new"), 0o600))
	oldInfo, err := os.Stat(oldPath)
	require.NoError(t, err)
	binding := agentsync.BoundedCoverageBinding{
		Key: "opencode:new", Agent: parser.AgentOpenCode,
		DBPath: newPath, PhysicalDBPath: newPath, Scope: filepath.Dir(newPath),
	}
	old := &boundedCoverageState{
		binding: binding, dbFile: oldInfo, nativeAdmitted: true, pollOwned: true,
	}
	coordinator := &sharedUnwatchedPollCoordinator{
		coverage:      failingBoundedCoverageAdmission{},
		coverageState: map[string]*boundedCoverageState{binding.Key: old},
	}
	_, err = coordinator.admitBoundedCoverage(t.Context(), []agentsync.BoundedCoverageBinding{binding}, true)
	require.Error(t, err)
	assert.False(t, old.frozen, "failed replacement must return the previous state to polling")
}

func TestPollingOwnershipRebuildPreservesOverlapAndNativeAdmission(t *testing.T) {
	coordinator := &sharedUnwatchedPollCoordinator{
		coverageState: make(map[string]*boundedCoverageState),
		ownedBindings: map[string]map[string]struct{}{
			"one": {"shared": {}, "one-only": {}},
			"two": {"shared": {}},
		},
	}
	coordinator.coverageState["shared"] = &boundedCoverageState{}
	coordinator.coverageState["one-only"] = &boundedCoverageState{}
	coordinator.coverageState["native"] = &boundedCoverageState{nativeAdmitted: true}
	coordinator.coverageState["retired"] = &boundedCoverageState{}
	coordinator.rebuildPollingOwnershipLocked()

	assert.True(t, coordinator.coverageState["shared"].pollOwned)
	assert.True(t, coordinator.coverageState["one-only"].pollOwned)
	assert.False(t, coordinator.coverageState["native"].pollOwned)
	_, exists := coordinator.coverageState["retired"]
	assert.False(t, exists)

	delete(coordinator.ownedBindings, "one")
	coordinator.rebuildPollingOwnershipLocked()
	assert.True(t, coordinator.coverageState["shared"].pollOwned)
	delete(coordinator.ownedBindings, "two")
	coordinator.rebuildPollingOwnershipLocked()
	if state := coordinator.coverageState["shared"]; state != nil {
		assert.False(t, state.pollOwned)
	}
}

// reconcileGroupsSequentially adapts a per-group fake to the grouped syncer
// interface, mirroring the engine contract pinned by
// TestReconcileProviderRootsGrouped*: every group is attempted in order and
// failures are joined.
func reconcileGroupsSequentially(
	ctx context.Context,
	groups []agentsync.ProviderRootsGroup,
	reconcile func(context.Context, parser.AgentType, []string) error,
) error {
	var errs []error
	for _, group := range groups {
		if err := reconcile(ctx, group.Agent, group.Roots); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type recordingUnwatchedPollSyncer struct {
	mu           sync.Mutex
	calls        [][]string
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

func (s *cancelBlockingUnwatchedPollSyncer) ReconcileProviderRoots(
	ctx context.Context, _ parser.AgentType, _ []string,
) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	s.started <- struct{}{}
	<-ctx.Done()
	s.canceled <- struct{}{}
	return ctx.Err()
}

func (s *cancelBlockingUnwatchedPollSyncer) ReconcileProviderRootsGrouped(
	ctx context.Context, groups []agentsync.ProviderRootsGroup,
) error {
	return reconcileGroupsSequentially(ctx, groups, s.ReconcileProviderRoots)
}

func (s *cancelBlockingUnwatchedPollSyncer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *blockingUnwatchedPollSyncer) ReconcileProviderRoots(
	_ context.Context, _ parser.AgentType, roots []string,
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

func (s *blockingUnwatchedPollSyncer) ReconcileProviderRootsGrouped(
	ctx context.Context, groups []agentsync.ProviderRootsGroup,
) error {
	return reconcileGroupsSequentially(ctx, groups, s.ReconcileProviderRoots)
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

func (s *recordingUnwatchedPollSyncer) ReconcileProviderRoots(
	_ context.Context, _ parser.AgentType, roots []string,
) error {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string(nil), roots...))
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return s.reconcileErr
}

func (s *recordingUnwatchedPollSyncer) ReconcileProviderRootsGrouped(
	ctx context.Context, groups []agentsync.ProviderRootsGroup,
) error {
	return reconcileGroupsSequentially(ctx, groups, s.ReconcileProviderRoots)
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
		t.Context(), syncer, ticks, func() {}, func(run func()) { run() }, nil, time.Now, time.After,
	)
	t.Cleanup(coordinator.Stop)
	parent := t.TempDir()
	rootA := requireExistingPollRoot(t, parent, "root-a")
	rootB := requireExistingPollRoot(t, parent, "root-b")
	rootC := requireExistingPollRoot(t, parent, "root-c")

	additions := [][]pollingScope{
		{{Root: rootB}, {Root: rootA}},
		{{Root: rootA}, {Root: rootC}},
		{{Root: rootC}, {Root: rootB}},
	}
	var wg sync.WaitGroup
	addErrors := make(chan error, len(additions))
	for i, scopes := range additions {
		wg.Go(func() {
			addErrors <- coordinator.AddObligation(pollingObligation{
				Key: fmt.Sprintf("direct-%d", i), Scopes: scopes,
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
		t.Context(), syncer, ticks, func() {}, func(run func()) { run() }, nil, time.Now, time.After,
	)
	t.Cleanup(coordinator.Stop)
	parent := t.TempDir()
	initial := requireExistingPollRoot(t, parent, "initial")
	runtime := requireExistingPollRoot(t, parent, "runtime")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "initial", Scopes: []pollingScope{{Root: initial}},
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "runtime", Scopes: []pollingScope{{Root: runtime}},
	}))

	ticks <- time.Now()
	requirePollWithin(t, syncer.wake, time.Second)

	assert.Equal(t, [][]string{{initial, runtime}}, syncer.snapshot())
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
		func(run func()) { run() }, nil, time.Now,
		func(d time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		},
	)
	t.Cleanup(coordinator.Stop)
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "provider-root", Scopes: []pollingScope{{Root: root}},
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
		func(run func()) { run() }, nil, time.Now,
		func(d time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		},
	)
	t.Cleanup(coordinator.Stop)
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: physical, Scopes: []pollingScope{{Root: configured}}, Probe: physical,
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
		func(run func()) { run() }, nil, time.Now,
		func(d time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		},
	)
	t.Cleanup(coordinator.Stop)
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: configured, Scopes: []pollingScope{{Root: configured}}, Probe: configured,
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: sessions, Scopes: []pollingScope{{Root: configured}}, Probe: sessions,
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
				{Key: "base", Scopes: []pollingScope{{Root: base}, {Root: unrelated}}},
				{Key: "nested", Scopes: []pollingScope{{Root: nested}}, Probe: missingProbe},
			},
			want: []string{unrelated},
		},
		{
			name: "blocked ancestor defers available descendant",
			obligations: []pollingObligation{
				{Key: "nested", Scopes: []pollingScope{{Root: nested}, {Root: unrelated}}},
				{Key: "base", Scopes: []pollingScope{{Root: base}}, Probe: filepath.Join(base, "gone")},
			},
			want: []string{unrelated},
		},
		{
			name: "available probes keep overlapping roots pollable",
			obligations: []pollingObligation{
				{Key: "base", Scopes: []pollingScope{{Root: base}}},
				{Key: "nested", Scopes: []pollingScope{{Root: nested}}, Probe: nested},
			},
			want: []string{base, nested},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, availableUnwatchedPollRootsFlat(tc.obligations))
		})
	}
}

// TestAvailableUnwatchedPollRootsBlocksMixedRelativeAndAbsoluteScopes pins the
// path-form parity between this gate and the engine: ReconcileWatchRoots
// expands requested roots against configured dirs in absolute form
// (cleanRootPath), so a blocked scope configured relative and a pollable root
// configured absolute (or vice versa) still overlap on the engine side. The
// daemon-side blocking check must compare the same form, or the poll
// reconciles the deferred scope authoritatively and tombstones its sessions.
func TestAvailableUnwatchedPollRootsBlocksMixedRelativeAndAbsoluteScopes(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(base)
	scope := requireExistingPollRoot(t, base, "scope")
	sub := requireExistingPollRoot(t, scope, "sub")
	missingProbe := filepath.Join(base, "missing-probe")

	tests := []struct {
		name        string
		obligations []pollingObligation
	}{
		{
			name: "relative blocked scope defers absolute descendant",
			obligations: []pollingObligation{
				{Key: "blocked", Scopes: []pollingScope{{Root: "scope"}}, Probe: missingProbe},
				{Key: "poll", Scopes: []pollingScope{{Root: sub}}},
			},
		},
		{
			name: "absolute blocked scope defers relative descendant",
			obligations: []pollingObligation{
				{Key: "blocked", Scopes: []pollingScope{{Root: scope}}, Probe: missingProbe},
				{Key: "poll", Scopes: []pollingScope{{Root: filepath.Join("scope", "sub")}}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, availableUnwatchedPollRootsFlat(tc.obligations),
				"a blocked scope must defer overlapping roots regardless of "+
					"the path form each side was configured with")
		})
	}
}

// TestAvailableUnwatchedPollRootsBlocksScopesUnderFilesystemRoot pins the
// filesystem-root edge: a blocked scope at the filesystem root ("/" on Unix,
// the volume root on Windows) already ends in the separator, so naive
// root+separator prefix matching never sees any candidate as its descendant.
func TestAvailableUnwatchedPollRootsBlocksScopesUnderFilesystemRoot(t *testing.T) {
	candidate := t.TempDir()
	fsRoot := filepath.VolumeName(candidate) + string(filepath.Separator)
	obligations := []pollingObligation{
		{Key: "blocked", Scopes: []pollingScope{{Root: fsRoot}},
			Probe: filepath.Join(candidate, "missing-probe")},
		{Key: "poll", Scopes: []pollingScope{{Root: candidate}}},
	}
	assert.Empty(t, availableUnwatchedPollRootsFlat(obligations),
		"a blocked filesystem-root scope must defer every candidate beneath it")
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
		{Key: "persistent:" + base, Scopes: []pollingScope{{Root: base}}, Probe: base},
		{Key: "nested-gate", Scopes: []pollingScope{{Root: nested}},
			Probe: filepath.Join(nested, "missing-subtree")},
	}
	groups := availableUnwatchedPollScopes(obligations)
	if err := pollUnwatchedScopesOnce(t.Context(), engine, groups); err != nil {
		t.Logf("pollUnwatchedScopesOnce: %v", err)
	}
	roots := availableUnwatchedPollRootsFlat(obligations)

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
		context.Background(), syncer, ticks, func() {}, func(run func()) { run() }, nil, time.Now,
		func(d time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Time{}
			return ch
		},
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
		Key: "initial", Scopes: []pollingScope{{Root: initial}},
	}))

	coordinator.requestPoll()
	assert.Equal(t, []string{initial},
		requireReceivePollRoots(t, syncer.started, time.Second))

	addResult := make(chan error, 1)
	go func() {
		addResult <- coordinator.AddObligation(pollingObligation{
			Key: "replacement", Scopes: []pollingScope{{Root: replacement}},
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
		func(run func()) { run() }, nil, time.Now, time.After,
	)
	t.Cleanup(func() {
		cancelParent()
		coordinator.Stop()
	})
	owned := requireExistingPollRoot(t, t.TempDir(), "owned")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "owned", Scopes: []pollingScope{{Root: owned}},
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
		func(run func()) { run() }, nil, time.Now, time.After,
	)
	t.Cleanup(func() {
		cancelParent()
		coordinator.Stop()
	})
	owned := requireExistingPollRoot(t, t.TempDir(), "owned")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "owned", Scopes: []pollingScope{{Root: owned}},
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
			Key: "late", Scopes: []pollingScope{{Root: "/late"}},
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
		t.Context(), syncer, ticks, func() {}, func(run func()) { run() }, nil, time.Now, time.After,
	)
	t.Cleanup(coordinator.Stop)
	parent := t.TempDir()
	recovered := requireExistingPollRoot(t, parent, "recovered")
	stillUnwatched := requireExistingPollRoot(t, parent, "still-unwatched")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "recovered-watch", Scopes: []pollingScope{{Root: recovered}},
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "still-unwatched", Scopes: []pollingScope{{Root: stillUnwatched}},
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
		t.Context(), syncer, ticks, func() {}, func(run func()) { run() }, nil, time.Now, time.After,
	)
	t.Cleanup(coordinator.Stop)
	parent := t.TempDir()
	shared := requireExistingPollRoot(t, parent, "shared")
	persistentOnly := requireExistingPollRoot(t, parent, "persistent-only")
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "pending", Scopes: []pollingScope{{Root: shared}},
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "persistent", Scopes: []pollingScope{{Root: shared}, {Root: persistentOnly}},
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
		t.Context(), syncer, ticks, func() {}, func(run func()) { run() }, nil, time.Now, time.After,
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
		context.Background(), syncer, ticks, func() {}, func(run func()) { run() }, nil, time.Now, time.After,
	)
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "owned", Scopes: []pollingScope{{Root: "/owned"}},
	}))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			coordinator.Stop()
		})
	}
	wg.Wait()

	assert.ErrorIs(t, coordinator.AddObligation(pollingObligation{
		Key: "late", Scopes: []pollingScope{{Root: "/late"}},
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
			}, time.Now, time.After,
		)
		start := make(chan struct{})
		addResult := make(chan error, 1)
		stopDone := make(chan struct{})
		root := fmt.Sprintf("/race-root-%d", i)
		go func() {
			<-start
			addResult <- coordinator.AddObligation(pollingObligation{
				Key: root, Scopes: []pollingScope{{Root: root}},
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
				Key:    fmt.Sprintf("/late-root-%d", i),
				Scopes: []pollingScope{{Root: fmt.Sprintf("/late-root-%d", i)}},
			}),
			errUnwatchedPollStopped,
		)
	}
}

// availableUnwatchedPollRootsFlat is a test helper that flattens the
// per-agent groups from availableUnwatchedPollScopes into one sorted []string.
func availableUnwatchedPollRootsFlat(obligations []pollingObligation) []string {
	groups := availableUnwatchedPollScopes(obligations)
	unique := make(map[string]struct{})
	for _, roots := range groups {
		for _, r := range roots {
			unique[r] = struct{}{}
		}
	}
	result := make([]string, 0, len(unique))
	for r := range unique {
		result = append(result, r)
	}
	slices.Sort(result)
	return result
}

// TestCrossAgentBlockingBothDirections verifies cross-agent deferral: the empty
// agent means "every provider" for deferral purposes, so blocking must be
// conservative in both directions.
//
//   - Direction A: a missing probe on an empty-agent obligation must also block
//     a named-agent candidate for the same root.
//   - Direction B: a missing probe on a named-agent obligation must also block
//     an empty-agent candidate for the same root.
//
// Without the fix, availableUnwatchedPollScopes only checks blocked[scope.Agent]
// for each candidate; cross-agent blocks (where the blocking obligation uses a
// different agent) are invisible and the candidate is polled despite the gate.
//
// Ancestor and descendant cases are included in both directions so that
// overlapsDeferredScope is load-bearing: plain string equality between the
// blocked root and the candidate root would leave those cases unprotected.
func TestCrossAgentBlockingBothDirections(t *testing.T) {
	parent := t.TempDir()
	dir := requireExistingPollRoot(t, parent, "shared-dir")
	childDir := requireExistingPollRoot(t, dir, "child")
	missingProbe := filepath.Join(parent, "missing-probe") // does not exist

	tests := []struct {
		name            string
		obligations     []pollingObligation
		wantAbsentAgent parser.AgentType
		wantAbsentRoot  string
	}{
		// Exact-root cases (original coverage, kept for regression).
		{
			name: "empty_agent_blocked_defers_named_agent_exact",
			obligations: []pollingObligation{
				{Key: "k1", Probe: missingProbe, Scopes: []pollingScope{{Agent: parser.AgentType(""), Root: dir}}},
				{Key: "k2", Scopes: []pollingScope{{Agent: parser.AgentGemini, Root: dir}}},
			},
			wantAbsentAgent: parser.AgentGemini,
			wantAbsentRoot:  dir,
		},
		{
			name: "named_agent_blocked_defers_empty_agent_exact",
			obligations: []pollingObligation{
				{Key: "k1", Probe: missingProbe, Scopes: []pollingScope{{Agent: parser.AgentGemini, Root: dir}}},
				{Key: "k2", Scopes: []pollingScope{{Agent: parser.AgentType(""), Root: dir}}},
			},
			wantAbsentAgent: parser.AgentType(""),
			wantAbsentRoot:  dir,
		},
		// Ancestor-blocked cases: the blocked scope's Root is an ancestor of the
		// candidate's Root. Plain string equality would miss this relationship;
		// overlapsDeferredScope must be called to detect it.
		{
			name: "empty_agent_ancestor_blocked_defers_named_agent_descendant",
			obligations: []pollingObligation{
				// empty agent blocked at parent; candidate is a child of parent
				{Key: "k1", Probe: missingProbe, Scopes: []pollingScope{{Agent: parser.AgentType(""), Root: dir}}},
				{Key: "k2", Scopes: []pollingScope{{Agent: parser.AgentGemini, Root: childDir}}},
			},
			wantAbsentAgent: parser.AgentGemini,
			wantAbsentRoot:  childDir,
		},
		{
			name: "named_agent_ancestor_blocked_defers_empty_agent_descendant",
			obligations: []pollingObligation{
				// named agent blocked at parent; candidate is a child of parent
				{Key: "k1", Probe: missingProbe, Scopes: []pollingScope{{Agent: parser.AgentGemini, Root: dir}}},
				{Key: "k2", Scopes: []pollingScope{{Agent: parser.AgentType(""), Root: childDir}}},
			},
			wantAbsentAgent: parser.AgentType(""),
			wantAbsentRoot:  childDir,
		},
		// Descendant-blocked cases: the blocked scope's Root is a descendant of
		// the candidate's Root. Plain string equality would miss this too.
		{
			name: "empty_agent_descendant_blocked_defers_named_agent_ancestor",
			obligations: []pollingObligation{
				// empty agent blocked at child; candidate is the parent of child
				{Key: "k1", Probe: missingProbe, Scopes: []pollingScope{{Agent: parser.AgentType(""), Root: childDir}}},
				{Key: "k2", Scopes: []pollingScope{{Agent: parser.AgentGemini, Root: dir}}},
			},
			wantAbsentAgent: parser.AgentGemini,
			wantAbsentRoot:  dir,
		},
		{
			name: "named_agent_descendant_blocked_defers_empty_agent_ancestor",
			obligations: []pollingObligation{
				// named agent blocked at child; candidate is the parent of child
				{Key: "k1", Probe: missingProbe, Scopes: []pollingScope{{Agent: parser.AgentGemini, Root: childDir}}},
				{Key: "k2", Scopes: []pollingScope{{Agent: parser.AgentType(""), Root: dir}}},
			},
			wantAbsentAgent: parser.AgentType(""),
			wantAbsentRoot:  dir,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := availableUnwatchedPollScopes(tc.obligations)
			roots := result[tc.wantAbsentAgent]
			assert.NotContains(t, roots, tc.wantAbsentRoot,
				"cross-agent blocking must prevent root from appearing in %q group",
				tc.wantAbsentAgent)
		})
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
