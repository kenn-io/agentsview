package main

// Tests for scoping degraded-coverage polling to one provider per pass.

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
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

		logMu.Lock()
		output := logBuf.String()
		logMu.Unlock()
		assert.Contains(t, output, "polling 2 unwatched root(s)",
			"log must report total root count, not per-group count")
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
		t.Fatal("expected after() to be called for cooldown wait")
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

	// Assert A was never called.
	assert.Never(t, func() bool {
		for _, c := range syncer.snapshot() {
			if c.Agent == parser.AgentClaude {
				return true
			}
		}
		return false
	}, 100*time.Millisecond, 10*time.Millisecond,
		"provider A must never be called while its probe is missing")
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
		t.Fatal("cooldown after() was not called")
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
		t.Fatal("Stop() did not return while in cooldown wait")
	}
}
