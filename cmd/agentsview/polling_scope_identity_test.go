package main

// Regression tests for provider identity in polling obligations.
// Tests are written test-first; they are expected to fail against the
// un-fixed head before the production-code changes are applied.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

// TestPollingScopesForDirsEmitsAllAgents covers the pending-dir branch: when two
// providers share the same configured syncDir, pollingScopesForDirs must emit
// one scope per agent, not collapse them to a single scope for whichever agent
// happens to appear last in the map iteration.
func TestPollingScopesForDirsEmitsAllAgents(t *testing.T) {
	parent := t.TempDir()
	sharedDir := filepath.Join(parent, "shared-sync")

	root := watchRoot{
		path: filepath.Join(parent, "physical-root"),
		scopes: []watchScope{
			{agent: parser.AgentClaude, syncDir: sharedDir},
			{agent: parser.AgentGemini, syncDir: sharedDir},
		},
	}

	scopes := root.pollingScopesForDirs([]string{sharedDir})

	agentSet := make(map[parser.AgentType]bool)
	for _, s := range scopes {
		agentSet[s.Agent] = true
	}
	assert.True(t, agentSet[parser.AgentClaude],
		"Claude scope must appear when two providers share a syncDir")
	assert.True(t, agentSet[parser.AgentGemini],
		"Gemini scope must appear when two providers share a syncDir")
	assert.Len(t, scopes, 2,
		"one scope per agent, not collapsed to a single scope")
}

// TestWatchPollingObligationsPersistentDirBothAgents covers the persistent-dir branch:
// when two agents share a persistent polling dir, the obligation for that dir
// must carry both agents' scopes, not just the last writer in syncDirToAgents.
// Uses a non-nil results slice so the "no watcher" path is not taken; only the
// persistent-dir loop runs.
func TestWatchPollingObligationsPersistentDirBothAgents(t *testing.T) {
	parent := t.TempDir()
	sharedDir := filepath.Join(parent, "shared-sync")

	roots := []watchRoot{{
		path: filepath.Join(parent, "physical-root"),
		scopes: []watchScope{
			{agent: parser.AgentClaude, syncDir: sharedDir},
			{agent: parser.AgentGemini, syncDir: sharedDir},
		},
		persistentPollingDirs: []string{sharedDir},
	}}
	// Non-nil results: root is watched and the lifecycle is owned, so the
	// "no watcher" (i >= len(results)) branch does NOT execute.
	results := []agentsync.RecursiveWatchResult{{Watched: 1, MissingRootLifecycleOwned: false}}

	got := watchPollingObligations(roots, results, nil, nil)

	agentSet := make(map[parser.AgentType]bool)
	for _, ob := range got {
		for _, scope := range ob.Scopes {
			if scope.Root == filepath.Clean(sharedDir) {
				agentSet[parser.AgentType(scope.Agent)] = true
			}
		}
	}
	assert.True(t, agentSet[parser.AgentClaude],
		"persistent obligation for shared dir must carry Claude's scope")
	assert.True(t, agentSet[parser.AgentGemini],
		"persistent obligation for shared dir must carry Gemini's scope")
}

// TestWatchPollingObligationsUnwatchedFallbackBothAgents covers the unwatched-fallback branch:
// when two agents share a dir in the unwatched-dirs fallback, both agents'
// scopes must appear in the resulting obligation. Uses non-nil results so the
// "no watcher" path is not taken.
func TestWatchPollingObligationsUnwatchedFallbackBothAgents(t *testing.T) {
	parent := t.TempDir()
	sharedDir := filepath.Join(parent, "shared-sync")

	roots := []watchRoot{{
		path: filepath.Join(parent, "physical-root"),
		scopes: []watchScope{
			{agent: parser.AgentClaude, syncDir: sharedDir},
			{agent: parser.AgentGemini, syncDir: sharedDir},
		},
	}}
	// Non-nil results so the "no watcher" branch is not taken.
	results := []agentsync.RecursiveWatchResult{{Watched: 1}}
	unwatchedDirs := []string{sharedDir}

	got := watchPollingObligations(roots, results, unwatchedDirs, nil)

	agentSet := make(map[parser.AgentType]bool)
	for _, ob := range got {
		for _, scope := range ob.Scopes {
			if scope.Root == filepath.Clean(sharedDir) {
				agentSet[parser.AgentType(scope.Agent)] = true
			}
		}
	}
	assert.True(t, agentSet[parser.AgentClaude],
		"unwatched-fallback obligation for shared dir must carry Claude's scope")
	assert.True(t, agentSet[parser.AgentGemini],
		"unwatched-fallback obligation for shared dir must carry Gemini's scope")
}

// TestRegisterWatcherUnavailablePreservesAgents: when the file watcher
// cannot be constructed, registerWatcherUnavailableObligations must NOT strip
// the agent field from each scope. Stripping makes per-agent blocking in the
// coordinator ineffective: a missing probe for Gemini would block the empty-
// agent group instead of Gemini's group, and healthy Claude changes would be
// frozen.
func TestRegisterWatcherUnavailablePreservesAgents(t *testing.T) {
	parent := t.TempDir()
	claudeDir := filepath.Join(parent, "claude-dir")
	require.NoError(t, os.Mkdir(claudeDir, 0o755))
	geminiDir := filepath.Join(parent, "gemini-dir")
	require.NoError(t, os.Mkdir(geminiDir, 0o755))

	var mu sync.Mutex
	var registered []agentsync.PollingObligation

	opts := agentsync.WatcherOptions{
		OnPollingRequired: func(ob agentsync.PollingObligation) error {
			mu.Lock()
			registered = append(registered, ob)
			mu.Unlock()
			return nil
		},
	}

	roots := []watchRoot{{
		path: filepath.Join(parent, "shared-root"),
		scopes: []watchScope{
			{agent: parser.AgentClaude, syncDir: claudeDir},
			{agent: parser.AgentGemini, syncDir: geminiDir},
		},
	}}

	err := registerWatcherUnavailableObligations(opts, roots, nil, nil)
	require.NoError(t, err)

	mu.Lock()
	obs := append([]agentsync.PollingObligation(nil), registered...)
	mu.Unlock()

	agentSet := make(map[string]bool)
	for _, ob := range obs {
		for _, scope := range ob.Scopes {
			agentSet[scope.Agent] = true
		}
	}
	assert.True(t, agentSet[string(parser.AgentClaude)],
		"Claude's agent must be preserved in watcher-unavailable obligations")
	assert.True(t, agentSet[string(parser.AgentGemini)],
		"Gemini's agent must be preserved in watcher-unavailable obligations")
	assert.False(t, agentSet[""],
		"no scope must be stripped to the empty agent; per-provider blocking requires real agents")
}

// TestWatchPollingObligationsPersistentDirCarriesAgentFromSymlinkOnlyProvider
// : when a provider's only physical root
// is a symlink and is therefore recorded in symlinkGatedDirs rather than roots,
// the persistent obligation for its syncDir must still carry the provider's
// agent. Without the fix, syncDirToAgents is built from roots only (empty in
// this case), so the persistent obligation gets Agent:"" and the coordinator
// cannot distinguish the provider's dir from an unowned fallback dir.
//
// This test uses the post-fix 4-parameter watchPollingObligations signature.
// The compile error is the red state: the function currently accepts 3 args.
func TestWatchPollingObligationsPersistentDirCarriesAgentFromSymlinkOnlyProvider(t *testing.T) {
	parent := t.TempDir()
	syncDir := filepath.Join(parent, "copilot-dir")
	require.NoError(t, os.Mkdir(syncDir, 0o755))
	symRoot := filepath.Join(parent, "sessions-symlink") // symlink root; not in roots

	// symlinkGatedDirs records that symRoot is a symlink root for
	// parser.AgentCopilot whose syncDir is syncDir.
	symlinkGated := map[string][]watchScope{
		symRoot: {{agent: parser.AgentCopilot, syncDir: syncDir}},
	}

	// roots is empty: the provider's only root is a symlink (in symlinkGatedDirs),
	// so nothing appears in the watch plan's regular root list.
	got := watchPollingObligations(
		nil, nil, []string{syncDir}, symlinkGated,
	)

	agentSet := make(map[string]bool)
	for _, ob := range got {
		for _, scope := range ob.Scopes {
			if scope.Root == filepath.Clean(syncDir) {
				agentSet[scope.Agent] = true
			}
		}
	}
	assert.True(t, agentSet[string(parser.AgentCopilot)],
		"persistent obligation for syncDir must carry the provider's agent "+
			"when its only root is a symlink recorded in symlinkGatedDirs")
	assert.False(t, agentSet[""],
		"no scope should carry the empty agent when provider identity is known from symlinkGatedDirs")
}

// TestWatcherStartFailureEmptyAgentBypassesNamedGate (coordinator level):
// level): after a backend Start failure, the coordinator may receive a generic
// "watcher-fallback" empty-agent obligation alongside the named per-provider
// obligations. Without cross-agent blocking, the empty-agent
// obligation bypasses the named provider's probe gate and reconciles a scope
// whose physical nested root is missing, tombstoning its sessions.
//
// The test installs a named Gemini obligation with a missing probe gate
// (simulating OnPollingRequired output for a Gemini root with a missing
// nested physical dir) alongside a watcher-fallback empty-agent obligation for
// both geminiDir and otherDir (simulating the OnCoverageDegraded output the
// current code emits on backend Start failure). After the fix, geminiDir must
// NOT be reconciled while the nested root is missing; otherDir must still run.
func TestWatcherStartFailureEmptyAgentBypassesNamedGate(t *testing.T) {
	parent := t.TempDir()
	nestedRoot := filepath.Join(parent, "gemini-nested") // does not exist
	geminiDir := requireExistingPollRoot(t, parent, "gemini-dir")
	otherDir := requireExistingPollRoot(t, parent, "other-dir")

	syncer := &recordingProviderPollSyncer{wake: make(chan struct{}, 4)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, make(chan time.Time), func() {},
		func(run func()) { run() }, nil, time.Now, time.After,
	)
	t.Cleanup(coordinator.Stop)

	// Named Gemini obligation with a missing probe gate — the nested physical
	// root (gemini-nested) is gone, so Gemini should be deferred.
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key:    pollingObligationKey("gemini", geminiDir),
		Scopes: []pollingScope{{Agent: parser.AgentGemini, Root: geminiDir}},
		Probe:  nestedRoot,
	}))
	// Simulates what OnCoverageDegraded("watcher-fallback") installs when the
	// current code calls it on backend Start failure even though
	// OnPollingRequired is also set: an empty-agent obligation covering every
	// registered root.
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "watcher-fallback",
		Scopes: []pollingScope{
			{Agent: parser.AgentType(""), Root: geminiDir},
			{Agent: parser.AgentType(""), Root: otherDir},
		},
	}))

	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)

	calls := syncer.snapshot()
	reconciledRoots := make(map[string]bool)
	for _, c := range calls {
		for _, r := range c.Roots {
			reconciledRoots[r] = true
		}
	}
	assert.False(t, reconciledRoots[geminiDir],
		"geminiDir must not be reconciled while its nested root is missing: "+
			"the empty-agent obligation must be blocked by cross-agent deferral")
	assert.True(t, reconciledRoots[otherDir],
		"otherDir must still be reconciled via the empty-agent obligation")
}

// TestSymlinkGateNamedProviderDefersOnlyThatProvider covers the coordinator level:
// a persistent obligation and a symlink-gate obligation for a named provider
// both carry that provider's agent. When the symlink probe is missing the
// coordinator must defer only that provider; a second, healthy provider must
// still be reconciled.
func TestSymlinkGateNamedProviderDefersOnlyThatProvider(t *testing.T) {
	parent := t.TempDir()
	claudeDir := requireExistingPollRoot(t, parent, "claude-dir")
	geminiDir := requireExistingPollRoot(t, parent, "gemini-dir")
	missingProbe := filepath.Join(parent, "gemini-sessions-target") // does not exist

	syncer := &recordingProviderPollSyncer{wake: make(chan struct{}, 4)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, make(chan time.Time), func() {},
		func(run func()) { run() }, nil, time.Now, time.After,
	)
	t.Cleanup(coordinator.Stop)

	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key:    "gemini-persistent",
		Scopes: []pollingScope{{Agent: parser.AgentGemini, Root: geminiDir}},
	}))
	// Symlink gate for Gemini: when probe is missing this must defer ONLY Gemini.
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key:    "gemini-symlink-gate",
		Scopes: []pollingScope{{Agent: parser.AgentGemini, Root: geminiDir}},
		Probe:  missingProbe,
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key:    "claude-persistent",
		Scopes: []pollingScope{{Agent: parser.AgentClaude, Root: claudeDir}},
	}))

	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)

	calls := syncer.snapshot()
	agents := make(map[parser.AgentType]bool)
	for _, c := range calls {
		agents[c.Agent] = true
	}
	assert.True(t, agents[parser.AgentClaude],
		"Claude must be reconciled; it is not affected by Gemini's broken symlink gate")
	assert.False(t, agents[parser.AgentGemini],
		"Gemini must NOT be reconciled while its symlink probe is missing")
}

// TestSharedSyncDirBothProvidersReconciled end to end: two providers
// sharing the same sync dir must each receive a ReconcileProviderRoots call
// once the dir is available, regardless of whether the path is in the pending
// or persistent branch.
func TestSharedSyncDirBothProvidersReconciled(t *testing.T) {
	parent := t.TempDir()
	sharedDir := requireExistingPollRoot(t, parent, "shared-sync")

	syncer := &recordingProviderPollSyncer{wake: make(chan struct{}, 4)}
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, make(chan time.Time), func() {},
		func(run func()) { run() }, nil, time.Now, time.After,
	)
	t.Cleanup(coordinator.Stop)

	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key: "shared-dir",
		Scopes: []pollingScope{
			{Agent: parser.AgentClaude, Root: sharedDir},
			{Agent: parser.AgentGemini, Root: sharedDir},
		},
	}))

	coordinator.requestPoll()
	requirePollWithin(t, syncer.wake, time.Second)
	requirePollWithin(t, syncer.wake, time.Second)

	calls := syncer.snapshot()
	agents := make(map[parser.AgentType]bool)
	for _, c := range calls {
		agents[c.Agent] = true
	}
	assert.True(t, agents[parser.AgentClaude],
		"Claude must receive ReconcileProviderRoots for the shared dir")
	assert.True(t, agents[parser.AgentGemini],
		"Gemini must receive ReconcileProviderRoots for the shared dir")
}

// TestUnwatchedPollCooldownIsExactlyOneInterval strengthens the
// existing cooldown invariant test:
//  1. The duration passed to after() must equal unwatchedPollInterval (not just
//     be positive — a 1ns wait would satisfy the weaker assertion).
//  2. No second reconcile call must start before the captured timer fires.
//  3. Exactly one second reconcile call must happen after the timer fires.
func TestUnwatchedPollCooldownIsExactlyOneInterval(t *testing.T) {
	ticks := make(chan time.Time, 2)
	passRelease := make(chan struct{})
	passStarted := make(chan struct{}, 1)
	t0 := time.Unix(1000, 0)
	now := t0

	var nowMu sync.Mutex
	getNow := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}

	// afterTimers carries the bidirectional channel so the test can fire the timer.
	afterTimers := make(chan chan time.Time, 4)
	var afterArgs []time.Duration
	var afterMu sync.Mutex
	getAfter := func(d time.Duration) <-chan time.Time {
		afterMu.Lock()
		afterArgs = append(afterArgs, d)
		afterMu.Unlock()
		ch := make(chan time.Time, 1)
		afterTimers <- ch
		return ch
	}

	firstCallDone := make(chan struct{})
	var callMu sync.Mutex
	var callCount int
	syncer := &manualProviderPollSyncer{
		fn: func(_ context.Context, _ parser.AgentType, _ []string) error {
			callMu.Lock()
			callCount++
			n := callCount
			callMu.Unlock()
			select {
			case passStarted <- struct{}{}:
			default:
			}
			if n == 1 {
				<-passRelease
				select {
				case firstCallDone <- struct{}{}:
				default:
				}
			}
			return nil
		},
	}

	root := requireExistingPollRoot(t, t.TempDir(), "root")
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, ticks, func() {},
		func(run func()) { run() }, nil,
		getNow, getAfter,
	)
	t.Cleanup(coordinator.Stop)

	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key:    "test-root",
		Scopes: []pollingScope{{Root: root}},
	}))

	// First tick starts the pass (which blocks).
	ticks <- t0
	select {
	case <-passStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first pass did not start before timeout")
	}

	// Second tick arrives while the first pass is running.
	ticks <- t0.Add(unwatchedPollInterval)

	// Advance "now" so that at completion the full interval has elapsed since
	// the tick deadline, making the cooldown remainder = unwatchedPollInterval.
	nowMu.Lock()
	now = t0.Add(unwatchedPollInterval + 500*time.Millisecond)
	nowMu.Unlock()

	// Release the first pass.
	close(passRelease)

	// Wait for the first pass to finish so lastCompletion is set.
	select {
	case <-firstCallDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first pass did not complete before timeout")
	}

	// The worker must call after() with exactly unwatchedPollInterval.
	var timerCh chan time.Time
	select {
	case timerCh = <-afterTimers:
	case <-time.After(2 * time.Second):
		t.Fatal("after() was not called for the cooldown wait")
	}

	afterMu.Lock()
	args := append([]time.Duration(nil), afterArgs...)
	afterMu.Unlock()
	require.Len(t, args, 1)
	assert.Equal(t, unwatchedPollInterval, args[0],
		"cooldown must wait exactly unwatchedPollInterval, not just a positive duration")

	// No second reconcile must have started before the timer fires.
	callMu.Lock()
	beforeFire := callCount
	callMu.Unlock()
	assert.Equal(t, 1, beforeFire,
		"no second reconcile must start before the cooldown timer fires (P10)")

	// Fire the cooldown timer.
	timerCh <- time.Now()

	// Exactly one second reconcile must start.
	select {
	case <-passStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("second reconcile did not start after the cooldown timer fired")
	}

	callMu.Lock()
	afterFire := callCount
	callMu.Unlock()
	assert.Equal(t, 2, afterFire,
		"exactly one second reconcile must fire after the cooldown timer (P10)")
}
