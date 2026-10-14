package main

// Tests for scoping degraded-coverage polling to one provider per pass.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// recordingProviderPollSyncer records ReconcileProviderRoots calls.
type recordingProviderPollSyncer struct {
	mu    sync.Mutex
	calls []providerPollCall
	wake  chan struct{}
	errs  map[parser.AgentType]error
}

type providerPollCall struct {
	Agent parser.AgentType
	Roots []string
}

func (s *recordingProviderPollSyncer) ReconcileProviderRoots(
	_ context.Context, agent parser.AgentType, roots []string,
) error {
	s.mu.Lock()
	s.calls = append(s.calls, providerPollCall{
		Agent: agent,
		Roots: append([]string(nil), roots...),
	})
	var err error
	if s.errs != nil {
		err = s.errs[agent]
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return err
}

func (s *recordingProviderPollSyncer) ReconcileProviderRootsGrouped(
	ctx context.Context, groups []agentsync.ProviderRootsGroup,
) error {
	return reconcileGroupsSequentially(ctx, groups, s.ReconcileProviderRoots)
}

func (s *recordingProviderPollSyncer) snapshot() []providerPollCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]providerPollCall, len(s.calls))
	for i, c := range s.calls {
		out[i] = providerPollCall{
			Agent: c.Agent,
			Roots: append([]string(nil), c.Roots...),
		}
	}
	return out
}

// groupedCountingPollSyncer records each grouped reconcile call verbatim.
type groupedCountingPollSyncer struct {
	mu        sync.Mutex
	calls     [][]agentsync.ProviderRootsGroup
	wake      chan struct{}
	reconcile func([]agentsync.ProviderRootsGroup) error
}

func (s *groupedCountingPollSyncer) ReconcileProviderRootsGrouped(
	_ context.Context, groups []agentsync.ProviderRootsGroup,
) error {
	copied := make([]agentsync.ProviderRootsGroup, len(groups))
	for i, group := range groups {
		copied[i] = agentsync.ProviderRootsGroup{
			Agent: group.Agent,
			Roots: append([]string(nil), group.Roots...),
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, copied)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	if s.reconcile != nil {
		return s.reconcile(groups)
	}
	return nil
}

func (s *groupedCountingPollSyncer) snapshot() [][]agentsync.ProviderRootsGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]agentsync.ProviderRootsGroup(nil), s.calls...)
}

func TestUnwatchedPollFolderRelease(t *testing.T) {
	for _, removed := range []string{"transcript", "project"} {
		t.Run(fmt.Sprintf("missing %s reconciled before release", removed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := filepath.Clean(t.TempDir())
				project := requireExistingPollRoot(t, root, "project")
				sourcePath := filepath.Join(project, "archived.jsonl")
				require.NoError(t, os.WriteFile(sourcePath, []byte(
					testjsonl.NewSessionBuilder().
						AddClaudeUser("2024-01-01T00:00:00Z", "archived question").
						String(),
				), 0o600))
				database := dbtest.OpenTestDB(t)
				const sessionID = "archived"
				engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentClaude: {root},
					},
					Machine: "local",
				})
				t.Cleanup(engine.Close)
				require.NoError(t, engine.ReconcileProviderRootsGrouped(
					t.Context(), []agentsync.ProviderRootsGroup{{Agent: parser.AgentClaude, Roots: []string{root}}},
				))
				stored, err := database.GetSessionFull(t.Context(), sessionID)
				require.NoError(t, err)
				require.NotNil(t, stored)
				assert.Nil(t, stored.SourceMissingAt)
				coordinator := newUnwatchedPollCoordinatorWithTicks(
					t.Context(), engine, make(chan time.Time), func() {},
					func(run func()) { run() }, nil, time.Now, time.After,
				)
				t.Cleanup(coordinator.Stop)
				obligation := agentsync.PollingObligation{
					Key:           agentsync.FSNotifyFolderPollingPrefix + root + ":" + project,
					Scopes:        []agentsync.PollingScope{{Agent: "claude", Root: project}},
					ReleaseScopes: []agentsync.PollingScope{{Agent: "claude", Root: root}},
					Probe:         project,
				}
				add := func(obligation agentsync.PollingObligation) error {
					return coordinator.AddObligation(syncObligationToPoller(obligation))
				}
				release := coordinator.RemoveObligation
				require.NoError(t, add(obligation))
				if removed == "project" {
					require.NoError(t, os.RemoveAll(project))
				} else {
					require.NoError(t, os.Remove(sourcePath))
				}
				coordinator.requestPoll()
				synctest.Wait()
				stored, err = database.GetSessionFull(t.Context(), sessionID)
				require.NoError(t, err)
				require.NotNil(t, stored)
				assert.Nil(t, stored.SourceMissingAt,
					"degraded polling cannot prove replacement-aware absence")
				// The release's root pass can check replacements before marking a source missing.
				require.NoError(t, release(obligation.Key))
				time.Sleep(unwatchedPollInterval)
				synctest.Wait()
				assert.Empty(t, coordinator.currentPollObligations())
				stored, err = database.GetSessionFull(t.Context(), sessionID)
				require.NoError(t, err)
				require.NotNil(t, stored)
				assert.NotNil(t, stored.SourceMissingAt,
					"final root reconciliation must mark the archived source missing")
			})
		})
	}
	t.Run("coordinator release state", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			folders      int
			missingProbe bool
			failed       bool
			superseded   bool
			replaced     bool
			mirrorWatch  bool
		}{
			{name: "eight releases coalesce into one root pass", folders: 8, mirrorWatch: true},
			{name: "folder recovery respects another provider's missing probe", folders: 1, missingProbe: true},
			{name: "folder release retries after a failed pass", folders: 1, failed: true},
			{name: "superseded folder disappears after failed pass", folders: 1, failed: true, superseded: true},
			{name: "new release survives completion of an older pass", folders: 1, replaced: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					root := t.TempDir()
					probe := filepath.Join(root, "tmp")
					release := make(chan struct{})
					started := make(chan struct{}, 1)
					var fail atomic.Bool
					fail.Store(tc.failed)
					syncer := &groupedCountingPollSyncer{wake: make(chan struct{}, 4)}
					syncer.reconcile = func([]agentsync.ProviderRootsGroup) error {
						if tc.replaced {
							started <- struct{}{}
							<-release
						}
						if fail.Load() {
							return os.ErrPermission
						}
						return nil
					}
					coordinator := newUnwatchedPollCoordinatorWithTicks(
						t.Context(), syncer, make(chan time.Time), func() {},
						func(run func()) { run() }, nil, time.Now, time.After,
					)
					t.Cleanup(func() {
						select {
						case <-release:
						default:
							close(release)
						}
						coordinator.Stop()
					})
					if tc.folders > 1 {
						// Hold releases until the cooldown ends so they share one root pass.
						coordinator.pollMu.Lock()
						coordinator.lastCompletion = time.Now()
						coordinator.pollMu.Unlock()
					}
					add := coordinator.AddObligation
					releaseObligation := coordinator.RemoveObligation
					if tc.mirrorWatch {
						options := archivePushWatchWatcherOptions(nil, coordinator)
						add = func(obligation pollingObligation) error {
							return options.OnPollingRequired(agentsync.PollingObligation{
								Key: obligation.Key, Probe: obligation.Probe,
								Scopes: []agentsync.PollingScope{
									{Agent: "claude", Root: obligation.Probe},
									{Agent: "codex", Root: root},
								},
								ReleaseScopes: []agentsync.PollingScope{
									{Agent: "claude", Root: root},
									{Agent: "codex", Root: root},
								},
							})
						}
						releaseObligation = options.OnPollingReleased
					}
					var folders []string
					var obligation pollingObligation
					for i := range tc.folders {
						folder := requireExistingPollRoot(t, root, fmt.Sprintf("project-%d", i))
						folders = append(folders, folder)
						obligation = pollingObligation{
							Key:    "fsnotify-folder:" + root + ":" + folder,
							Scopes: []pollingScope{{Agent: parser.AgentClaude, Root: folder}},
							Probe:  folder,
						}
						if tc.folders > 1 {
							obligation.Scopes = append(obligation.Scopes, pollingScope{Agent: parser.AgentCodex, Root: root})
							obligation.ReleaseScopes = []pollingScope{
								{Agent: parser.AgentClaude, Root: root},
								{Agent: parser.AgentCodex, Root: root},
							}
						}
						if tc.missingProbe {
							obligation.Scopes = append(obligation.Scopes, pollingScope{Agent: parser.AgentGemini, Root: root})
							require.NoError(t, coordinator.AddObligation(pollingObligation{
								Key:    "gemini",
								Scopes: []pollingScope{{Agent: parser.AgentGemini, Root: root}},
								Probe:  probe,
							}))
						}
						if tc.superseded {
							require.NoError(t, coordinator.AddObligation(pollingObligation{
								Key:    "fsnotify-runtime:" + root,
								Scopes: []pollingScope{{Agent: parser.AgentClaude, Root: root}},
								Probe:  root,
							}))
						}
						require.NoError(t, add(obligation))
						require.NoError(t, releaseObligation(obligation.Key))
					}
					want := []agentsync.ProviderRootsGroup{{Agent: parser.AgentClaude, Roots: folders}}
					if tc.folders > 1 {
						synctest.Wait()
						assert.Empty(t, syncer.snapshot(), "folder releases must respect the poll cooldown")
						time.Sleep(unwatchedPollInterval)
						want[0].Roots = []string{root}
						want = append(want, agentsync.ProviderRootsGroup{Agent: parser.AgentCodex, Roots: []string{root}})
					}
					if tc.superseded {
						want[0].Roots = []string{root, folders[0]}
					}
					if tc.replaced {
						requirePollWithin(t, started, time.Second)
						require.NoError(t, add(obligation))
						require.NoError(t, releaseObligation(obligation.Key))
						close(release)
					}
					synctest.Wait()
					assert.Equal(t, [][]agentsync.ProviderRootsGroup{want}, syncer.snapshot(),
						"one pass must reconcile every released scope together")
					if tc.folders > 1 {
						assert.Empty(t, coordinator.currentPollObligations())
						return
					}
					remaining := 1
					if tc.missingProbe || tc.superseded {
						remaining = 2
					}
					require.Len(t, coordinator.currentPollObligations(), remaining)
					for _, pending := range coordinator.currentPollObligations() {
						if pending.Key == obligation.Key {
							assert.True(t, pending.releasePending,
								"an incomplete or replaced release must retain polling ownership")
						}
					}
					if tc.missingProbe {
						require.NoError(t, os.Mkdir(probe, 0o755))
						want = append(want, agentsync.ProviderRootsGroup{Agent: parser.AgentGemini, Roots: []string{root}})
					}
					if tc.superseded {
						require.NoError(t, os.Remove(folders[0]))
						want[0].Roots = []string{root}
					}
					fail.Store(false)
					coordinator.requestPoll()
					time.Sleep(unwatchedPollInterval)
					synctest.Wait()
					calls := syncer.snapshot()
					require.Len(t, calls, 2)
					assert.Equal(t, want, calls[1], "the retry must finish the pending release scopes")
					if tc.missingProbe || tc.superseded {
						pending := coordinator.currentPollObligations()
						require.Len(t, pending, 1)
						key := "gemini"
						if tc.superseded {
							key = "fsnotify-runtime:" + root
						}
						assert.Equal(t, key, pending[0].Key, "another obligation must survive the folder release")
					} else {
						assert.Empty(t, coordinator.currentPollObligations())
					}
				})
			})
		}
	})
}

// TestUnwatchedPollIssuesOneGroupedReconcilePerPass is the cardinality
// regression for the shared epilogue: a poll pass must hand every provider
// group to the engine in a single grouped call, so the engine's archive-sized
// per-pass work (skip-cache persistence, global subagent linking) stays
// constant as the number of providers holding obligations grows.
func TestUnwatchedPollIssuesOneGroupedReconcilePerPass(t *testing.T) {
	for _, providerCount := range []int{2, 8} {
		t.Run(fmt.Sprintf("providers=%d", providerCount), func(t *testing.T) {
			parent := t.TempDir()
			syncer := &groupedCountingPollSyncer{wake: make(chan struct{}, 1)}
			coordinator := newUnwatchedPollCoordinatorWithTicks(
				t.Context(), syncer, make(chan time.Time), func() {},
				func(run func()) { run() }, nil,
				time.Now, time.After,
			)
			t.Cleanup(coordinator.Stop)

			for i := range providerCount {
				root := requireExistingPollRoot(t, parent, fmt.Sprintf("root-%d", i))
				agent := parser.AgentType(fmt.Sprintf("agent-%d", i))
				require.NoError(t, coordinator.AddObligation(pollingObligation{
					Key:    fmt.Sprintf("degraded:%s:%s", agent, root),
					Scopes: []pollingScope{{Agent: agent, Root: root}},
					Probe:  root,
				}))
			}

			coordinator.requestPoll()
			requirePollWithin(t, syncer.wake, time.Second)

			calls := syncer.snapshot()
			require.Len(t, calls, 1,
				"a pass must issue exactly one grouped reconcile call, "+
					"independent of provider count")
			assert.Len(t, calls[0], providerCount,
				"the single grouped call must cover every provider's group")
		})
	}
}

// TestUnwatchedPollDoesNotDragUnrelatedProvidersThroughOneProvidersGap is the
// reproduction test: one provider's degraded coverage must not cause an
// authoritative pass for any other provider.
func TestUnwatchedPollDoesNotDragUnrelatedProvidersThroughOneProvidersGap(t *testing.T) {
	t.Run("main", func(t *testing.T) {
		parent := t.TempDir()
		rootA := requireExistingPollRoot(t, parent, "root-a")
		rootB := requireExistingPollRoot(t, parent, "root-b")

		syncer := &recordingProviderPollSyncer{wake: make(chan struct{}, 4)}
		coordinator := newUnwatchedPollCoordinatorWithTicks(
			t.Context(), syncer, make(chan time.Time), func() {},
			func(run func()) { run() }, nil,
			time.Now, time.After,
		)
		t.Cleanup(coordinator.Stop)

		require.NoError(t, coordinator.AddObligation(pollingObligation{
			Key: "degraded:agentA:" + rootA,
			Scopes: []pollingScope{
				{Agent: parser.AgentClaude, Root: rootA},
			},
			Probe: rootA,
		}))
		require.NoError(t, coordinator.AddObligation(pollingObligation{
			Key: "degraded:agentB:" + rootB,
			Scopes: []pollingScope{
				{Agent: parser.AgentOpenHands, Root: rootB},
			},
			Probe: rootB,
		}))

		coordinator.requestPoll()
		requirePollWithin(t, syncer.wake, time.Second)
		requirePollWithin(t, syncer.wake, time.Second)

		calls := syncer.snapshot()
		require.Len(t, calls, 2,
			"each provider must get exactly one ReconcileProviderRoots call")
		var agentACalls, agentBCalls []providerPollCall
		for _, c := range calls {
			switch c.Agent {
			case parser.AgentClaude:
				agentACalls = append(agentACalls, c)
			case parser.AgentOpenHands:
				agentBCalls = append(agentBCalls, c)
			default:
				require.FailNowf(t, "unexpected agent", "got %v", c.Agent)
			}
		}
		require.Len(t, agentACalls, 1, "provider A must have exactly one call")
		assert.Equal(t, []string{rootA}, agentACalls[0].Roots,
			"provider A's call must contain only A's root")
		require.Len(t, agentBCalls, 1, "provider B must have exactly one call")
		assert.Equal(t, []string{rootB}, agentBCalls[0].Roots,
			"provider B must not see provider A's root")
	})

	// log_reports_total_roots: the log line must report the TOTAL root count
	// across all groups, not a per-group count.
	t.Run("log_reports_total_roots", func(t *testing.T) {
		parent := t.TempDir()
		rootA := requireExistingPollRoot(t, parent, "root-a")
		rootB := requireExistingPollRoot(t, parent, "root-b")

		syncer := &recordingProviderPollSyncer{wake: make(chan struct{}, 4)}
		var logMu sync.Mutex
		origOutput := log.Writer()
		var logBuf strings.Builder
		log.SetOutput(&lockedWriter{mu: &logMu, w: &logBuf})
		t.Cleanup(func() { log.SetOutput(origOutput) })

		coordinator := newUnwatchedPollCoordinatorWithTicks(
			t.Context(), syncer, make(chan time.Time), func() {},
			func(run func()) { run() }, nil,
			time.Now, time.After,
		)
		t.Cleanup(coordinator.Stop)

		require.NoError(t, coordinator.AddObligation(pollingObligation{
			Key:    "degraded:agentA:" + rootA,
			Scopes: []pollingScope{{Agent: parser.AgentClaude, Root: rootA}},
			Probe:  rootA,
		}))
		require.NoError(t, coordinator.AddObligation(pollingObligation{
			Key:    "degraded:agentB:" + rootB,
			Scopes: []pollingScope{{Agent: parser.AgentOpenHands, Root: rootB}},
			Probe:  rootB,
		}))

		coordinator.requestPoll()
		requirePollWithin(t, syncer.wake, time.Second)
		requirePollWithin(t, syncer.wake, time.Second)

		coordinator.Stop()
		logMu.Lock()
		output := logBuf.String()
		logMu.Unlock()
		assert.Contains(t, output, "polling 2 unwatched root(s)",
			"log must report total root count, not per-group count")
		assert.Contains(t, output, fmt.Sprintf("claude=[%s]", rootA))
		assert.Contains(t, output, fmt.Sprintf("openhands=[%s]", rootB))
		assert.Contains(t, output, "polled 2 unwatched root(s) in ")
	})
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *strings.Builder
}

func (lw *lockedWriter) Write(p []byte) (n int, err error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

// TestUnwatchedPollWaitsAfterAPassLongerThanTheInterval asserts that if the
// previous pass took longer than the interval, the worker waits until
// lastCompletion + interval before starting the next pass.
func TestUnwatchedPollWaitsAfterAPassLongerThanTheInterval(t *testing.T) {
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

	afterCh := make(chan (<-chan time.Time), 4)
	var afterArgs []time.Duration
	var afterMu sync.Mutex
	getAfter := func(d time.Duration) <-chan time.Time {
		afterMu.Lock()
		afterArgs = append(afterArgs, d)
		afterMu.Unlock()
		ch := make(chan time.Time, 1)
		afterCh <- ch
		return ch
	}

	firstCall := make(chan struct{}, 1)
	callCount := 0
	var callMu sync.Mutex
	syncer := &manualProviderPollSyncer{
		fn: func(_ context.Context, _ parser.AgentType, _ []string) error {
			callMu.Lock()
			callCount++
			n := callCount
			callMu.Unlock()
			if n == 1 {
				select {
				case firstCall <- struct{}{}:
				default:
				}
				<-passRelease
			}
			select {
			case passStarted <- struct{}{}:
			default:
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

	// First tick starts the pass.
	ticks <- t0
	requirePollWithin(t, firstCall, time.Second)

	// Second tick arrives while the pass is running; gets buffered.
	ticks <- t0.Add(unwatchedPollInterval)

	// Advance "now" to simulate the pass taking longer than the interval.
	nowMu.Lock()
	now = t0.Add(unwatchedPollInterval + 500*time.Millisecond)
	nowMu.Unlock()

	// Release the first pass.
	close(passRelease)

	// The worker should call after() with a positive remaining duration.
	select {
	case <-afterCh:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "expected after() to be called for cooldown wait")
	}

	afterMu.Lock()
	args := append([]time.Duration(nil), afterArgs...)
	afterMu.Unlock()
	require.Len(t, args, 1)
	assert.Greater(t, args[0], time.Duration(0),
		"the cooldown must wait a positive duration when the pass ran over the interval")
}

type manualProviderPollSyncer struct {
	fn func(context.Context, parser.AgentType, []string) error
}

func (s *manualProviderPollSyncer) ReconcileProviderRoots(
	ctx context.Context, agent parser.AgentType, roots []string,
) error {
	return s.fn(ctx, agent, roots)
}

func (s *manualProviderPollSyncer) ReconcileProviderRootsGrouped(
	ctx context.Context, groups []agentsync.ProviderRootsGroup,
) error {
	return reconcileGroupsSequentially(ctx, groups, s.ReconcileProviderRoots)
}

// TestUnwatchedPollAttemptsEveryProviderAfterOneFails asserts that when the
// first provider group errors, subsequent groups are still attempted once.
func TestUnwatchedPollAttemptsEveryProviderAfterOneFails(t *testing.T) {
	parent := t.TempDir()
	rootA := requireExistingPollRoot(t, parent, "root-a")
	rootB := requireExistingPollRoot(t, parent, "root-b")
	rootC := requireExistingPollRoot(t, parent, "root-c")

	wantErr := errors.New("agent-a failed")
	syncer := &recordingProviderPollSyncer{
		wake: make(chan struct{}, 8),
		errs: map[parser.AgentType]error{
			parser.AgentClaude: wantErr,
		},
	}

	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, make(chan time.Time), func() {},
		func(run func()) { run() }, nil,
		time.Now, time.After,
	)
	t.Cleanup(coordinator.Stop)

	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key:    "agent-a-root",
		Scopes: []pollingScope{{Agent: parser.AgentClaude, Root: rootA}},
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key:    "agent-b-root",
		Scopes: []pollingScope{{Agent: parser.AgentOpenHands, Root: rootB}},
	}))
	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key:    "agent-c-root",
		Scopes: []pollingScope{{Agent: parser.AgentDevin, Root: rootC}},
	}))

	coordinator.requestPoll()
	// Wait for all three groups to be attempted.
	requirePollWithin(t, syncer.wake, time.Second)
	requirePollWithin(t, syncer.wake, time.Second)
	requirePollWithin(t, syncer.wake, time.Second)

	calls := syncer.snapshot()
	assert.Len(t, calls, 3,
		"all three providers must be attempted even when agent-a fails")

	agents := make(map[parser.AgentType]bool)
	for _, c := range calls {
		agents[c.Agent] = true
	}
	assert.True(t, agents[parser.AgentClaude], "agent-a must be attempted")
	assert.True(t, agents[parser.AgentOpenHands], "agent-b must be attempted even after agent-a's error")
	assert.True(t, agents[parser.AgentDevin], "agent-c must be attempted even after agent-a's error")
}

// TestUnwatchedPollDefersOnlyTheProviderWhoseProbeIsMissing asserts that a
// missing probe defers only its own provider; the healthy provider still polls.
func TestUnwatchedPollDefersOnlyTheProviderWhoseProbeIsMissing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := t.TempDir()
		sharedRoot := requireExistingPollRoot(t, parent, "shared")
		probeA := filepath.Join(sharedRoot, "probe-a")
		require.NoError(t, os.Mkdir(probeA, 0o755))
		// probeB is intentionally absent — A's probe is missing.

		syncer := &recordingProviderPollSyncer{wake: make(chan struct{}, 4)}
		coordinator := newUnwatchedPollCoordinatorWithTicks(
			t.Context(), syncer, make(chan time.Time), func() {},
			func(run func()) { run() }, nil,
			time.Now, time.After,
		)
		t.Cleanup(coordinator.Stop)

		// Provider A: probe is missing → must be deferred.
		missingProbeA := filepath.Join(sharedRoot, "missing-probe-a")
		require.NoError(t, coordinator.AddObligation(pollingObligation{
			Key:    "agent-a-root",
			Scopes: []pollingScope{{Agent: parser.AgentClaude, Root: sharedRoot}},
			Probe:  missingProbeA,
		}))
		// Provider B: probe is present → must be polled.
		require.NoError(t, coordinator.AddObligation(pollingObligation{
			Key:    "agent-b-root",
			Scopes: []pollingScope{{Agent: parser.AgentOpenHands, Root: sharedRoot}},
			Probe:  probeA, // present
		}))

		coordinator.requestPoll()
		requirePollWithin(t, syncer.wake, time.Second)

		calls := syncer.snapshot()
		require.Len(t, calls, 1,
			"only the healthy provider must be called; the one with missing probe must be deferred")
		assert.Equal(t, parser.AgentOpenHands, calls[0].Agent,
			"the call must be for the healthy provider")
		assert.Equal(t, []string{sharedRoot}, calls[0].Roots)
	})
}

// TestUnwatchedPollStopDuringCooldown asserts that Stop() returns immediately
// during the cooldown wait rather than waiting out the cooldown.
func TestUnwatchedPollStopDuringCooldown(t *testing.T) {
	ticks := make(chan time.Time, 1)
	passRelease := make(chan struct{})
	passStarted := make(chan struct{}, 1)

	t0 := time.Unix(2000, 0)
	getNow := func() time.Time { return t0.Add(10 * time.Millisecond) }

	// after() blocks until Stop cancels the context.
	afterBlocking := make(chan (<-chan time.Time), 1)
	getAfter := func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time) // never fires
		afterBlocking <- ch
		return ch
	}

	syncer := &manualProviderPollSyncer{
		fn: func(_ context.Context, _ parser.AgentType, _ []string) error {
			select {
			case passStarted <- struct{}{}:
			default:
			}
			<-passRelease
			return nil
		},
	}

	root := requireExistingPollRoot(t, t.TempDir(), "root")
	coordinator := newUnwatchedPollCoordinatorWithTicks(
		t.Context(), syncer, ticks, func() {},
		func(run func()) { run() }, nil,
		getNow, getAfter,
	)
	t.Cleanup(func() {
		select {
		case <-passRelease:
		default:
			close(passRelease)
		}
		coordinator.Stop()
	})

	require.NoError(t, coordinator.AddObligation(pollingObligation{
		Key:    "root",
		Scopes: []pollingScope{{Root: root}},
	}))

	// First tick: start a pass.
	ticks <- t0
	requirePollWithin(t, passStarted, time.Second)

	// Second tick: buffer a wake. This will trigger the cooldown when the
	// first pass finishes.
	ticks <- t0.Add(time.Millisecond)

	// Release the first pass. The worker now enters the cooldown wait (after()).
	close(passRelease)

	// Wait for after() to be called.
	select {
	case <-afterBlocking:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "cooldown after() was not called")
	}

	// Stop must return without waiting out the cooldown.
	stopDone := make(chan struct{})
	go func() {
		coordinator.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "Stop() did not return while in cooldown wait")
	}
}
