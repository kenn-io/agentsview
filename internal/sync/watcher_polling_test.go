package sync

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWatcherStartFailureSuppressesFallbackWhenPollingOwnershipSet verifies
// that when OnPollingRequired is set and backend Start fails, named obligations
// are emitted for every registered root (carrying per-agent identity) and the
// generic empty-agent OnCoverageDegraded fallback is NOT called. Calling
// OnCoverageDegraded in addition would bypass per-agent probe gates.
func TestWatcherStartFailureSuppressesFallbackWhenPollingOwnershipSet(t *testing.T) {
	backend := newFakeWatchBackend()
	backend.startErr = errors.New("backend start failed")

	var degradedCalled bool
	var emitted []PollingObligation

	w, err := newWatcherWithBackendOptions(
		0, 0,
		func(_ context.Context, _ WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{
			OnCoverageDegraded: func(_ []string) error {
				degradedCalled = true
				return nil
			},
			OnPollingRequired: func(o PollingObligation) error {
				emitted = append(emitted, o)
				return nil
			},
		},
	)
	require.NoError(t, err)

	// Register roots so rootAgents is populated and the start-failure path
	// has something to emit.
	w.RegisterRoots([]WatchRoot{{
		Path:      "/gemini",
		Recursive: true,
		Exists:    true,
		Scopes:    []WatchScope{{Agent: "gemini", SyncDir: "/gemini-dir"}},
	}}, 100)

	startErr := w.Start()
	require.Error(t, startErr, "Start must return an error when backend fails")

	// OnCoverageDegraded must not be called; OnPollingRequired is the
	// authoritative coverage path when it is set.
	assert.False(t, degradedCalled,
		"OnCoverageDegraded must not be called when OnPollingRequired is set (P1-B)")

	// Named obligations must be emitted for all registered roots.
	require.Len(t, emitted, 1,
		"one obligation must be emitted per registered root on start failure")
	ob := emitted[0]
	assert.Equal(t, "startfailure:"+filepath.Clean("/gemini"), ob.Key,
		"obligation key must use startfailure: prefix to avoid colliding with caller obligations")
	assert.Equal(t, filepath.Clean("/gemini"), ob.Probe,
		"probe must be the registered root path")
	require.Len(t, ob.Scopes, 1,
		"obligation must carry one scope per named agent")
	assert.Equal(t, "gemini", ob.Scopes[0].Agent,
		"scope must carry the provider agent identity from rootAgents")
	assert.False(t, ob.Scopes[0].Agent == "",
		"named-agent root must not produce an empty-agent scope")
}

// TestWatcherStartFailureEmitsNamedObligationForCleanRoot verifies the P1
// regression: when OnPollingRequired is set and a root registered cleanly
// (healthy result, no pending dirs), the backend's Start failure must still
// emit a named obligation covering that root. The caller's pre-start
// obligations (from watchPollingObligations) only cover roots with known
// problems; a clean root has none, so start-failure coverage is the only
// fallback.
//
// Crucially, the emitted scope's Root must be the configured SyncDir, not the
// physical watch path. When the physical path and configured dir do not overlap
// in either direction (the common case for nested provider roots), using the
// physical path leaves the configured dir with no authoritative polling
// coverage.
func TestWatcherStartFailureEmitsNamedObligationForCleanRoot(t *testing.T) {
	backend := newFakeWatchBackend()
	backend.startErr = errors.New("backend start failed")

	var emitted []PollingObligation
	w, err := newWatcherWithBackendOptions(
		0, 0,
		func(_ context.Context, _ WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{
			OnPollingRequired: func(o PollingObligation) error {
				emitted = append(emitted, o)
				return nil
			},
		},
	)
	require.NoError(t, err)

	w.RegisterRoots([]WatchRoot{{
		Path:      "/claude",
		Recursive: true,
		Exists:    true,
		Scopes:    []WatchScope{{Agent: "claude", SyncDir: "/claude-dir"}},
	}}, 100)

	startErr := w.Start()
	require.Error(t, startErr)

	require.Len(t, emitted, 1,
		"start failure must emit one obligation for the cleanly-registered root")
	ob := emitted[0]
	assert.Equal(t, filepath.Clean("/claude"), ob.Probe)
	hasClaudeAgent := slices.ContainsFunc(ob.Scopes, func(s PollingScope) bool {
		return s.Agent == "claude"
	})
	assert.True(t, hasClaudeAgent,
		"obligation must carry the provider's agent identity")
	// The scope Root must be the configured SyncDir, not the physical watch path.
	// Physical path /claude and configured dir /claude-dir do not overlap in
	// either direction; using /claude instead of /claude-dir leaves the
	// configured dir with no authoritative polling coverage.
	claudeScope, found := func() (PollingScope, bool) {
		for _, s := range ob.Scopes {
			if s.Agent == "claude" {
				return s, true
			}
		}
		return PollingScope{}, false
	}()
	require.True(t, found, "claude agent scope must be present")
	assert.Equal(t, filepath.Clean("/claude-dir"), filepath.Clean(claudeScope.Root),
		"scope Root must be the configured SyncDir (/claude-dir), not the physical watch path (/claude)")
}

// TestWatcherStartFailureEmitsEmptyAgentScopeForNoAgentRoot verifies that when
// a registered root has no named agents in rootScopes (e.g., all scopes had an
// empty agent string), the start-failure path still emits an obligation
// covering it with an empty-agent scope so that root is never silently dropped.
func TestWatcherStartFailureEmitsEmptyAgentScopeForNoAgentRoot(t *testing.T) {
	backend := newFakeWatchBackend()
	backend.startErr = errors.New("backend start failed")

	var emitted []PollingObligation
	w, err := newWatcherWithBackendOptions(
		0, 0,
		func(_ context.Context, _ WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{
			OnPollingRequired: func(o PollingObligation) error {
				emitted = append(emitted, o)
				return nil
			},
		},
	)
	require.NoError(t, err)

	// Register with no scopes so rootScopes[path] = [] (no named agents).
	w.RegisterRoots([]WatchRoot{{
		Path:   "/no-agent-root",
		Exists: true,
		Scopes: nil,
	}}, 100)

	startErr := w.Start()
	require.Error(t, startErr)

	require.Len(t, emitted, 1,
		"start failure must emit an obligation even for a root with no named agents")
	ob := emitted[0]
	require.Len(t, ob.Scopes, 1,
		"absent-agent root must still emit one empty-agent scope for coverage")
	assert.Equal(t, "", ob.Scopes[0].Agent,
		"scope must use empty agent when rootScopes has no named agents for the root")
}

// TestWatcherStartFailureNoEmptyAgentForNamedRoot verifies that when
// OnPollingRequired is set and a root has a named agent, the start-failure
// emission does not produce an unscoped empty-agent obligation for that root.
// An empty-agent scope would bypass per-agent probe gates and could let polling
// authoritatively reconcile a scope whose nested root is missing, tombstoning
// every session beneath it.
func TestWatcherStartFailureNoEmptyAgentForNamedRoot(t *testing.T) {
	backend := newFakeWatchBackend()
	backend.startErr = errors.New("backend start failed")

	var emitted []PollingObligation
	w, err := newWatcherWithBackendOptions(
		0, 0,
		func(_ context.Context, _ WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{
			OnPollingRequired: func(o PollingObligation) error {
				emitted = append(emitted, o)
				return nil
			},
		},
	)
	require.NoError(t, err)

	w.RegisterRoots([]WatchRoot{{
		Path:      "/gemini",
		Recursive: true,
		Exists:    true,
		Scopes:    []WatchScope{{Agent: "gemini", SyncDir: "/gemini-dir"}},
	}}, 100)

	startErr := w.Start()
	require.Error(t, startErr)

	require.NotEmpty(t, emitted,
		"start failure must emit at least one obligation for the named-agent root")
	for _, ob := range emitted {
		for _, scope := range ob.Scopes {
			assert.NotEmpty(t, scope.Agent,
				"start failure must not emit empty-agent scope for a root with a named agent")
		}
	}
}

// TestWatcherStartFailureMixedScopesBothDirsAreCovered verifies the P1
// Failure 2 regression: when a registered root has both a named-agent scope
// and an empty-agent scope, the start-failure emission must carry BOTH
// configured dirs and must NOT suppress the empty-agent scope just because a
// named agent also exists on the same physical root. Before the fix,
// RegisterRoots only stored named agents in rootAgents, discarding empty-agent
// scopes entirely, so /legacy-dir was silently dropped.
func TestWatcherStartFailureMixedScopesBothDirsAreCovered(t *testing.T) {
	backend := newFakeWatchBackend()
	backend.startErr = errors.New("backend start failed")

	var emitted []PollingObligation
	w, err := newWatcherWithBackendOptions(
		0, 0,
		func(_ context.Context, _ WatchBatch) error { return nil },
		backend, 8, 1_000,
		WatcherOptions{
			OnPollingRequired: func(o PollingObligation) error {
				emitted = append(emitted, o)
				return nil
			},
		},
	)
	require.NoError(t, err)

	w.RegisterRoots([]WatchRoot{{
		Path:      "/watch",
		Recursive: true,
		Exists:    true,
		Scopes: []WatchScope{
			{Agent: "gemini", SyncDir: "/gemini-dir"},
			{Agent: "", SyncDir: "/legacy-dir"},
		},
	}}, 100)

	startErr := w.Start()
	require.Error(t, startErr)

	require.Len(t, emitted, 1, "one obligation per physical root")
	ob := emitted[0]

	// Both configured dirs must appear in the emitted scopes.
	var geminiRoot, legacyRoot string
	for _, s := range ob.Scopes {
		switch s.Agent {
		case "gemini":
			geminiRoot = s.Root
		case "":
			legacyRoot = s.Root
		}
	}
	assert.Equal(t, filepath.Clean("/gemini-dir"), filepath.Clean(geminiRoot),
		"gemini scope Root must be the configured SyncDir /gemini-dir, not the physical path")
	assert.NotEmpty(t, legacyRoot,
		"empty-agent scope must not be dropped when a named agent coexists on the same root (P1 Failure 2)")
	assert.Equal(t, filepath.Clean("/legacy-dir"), filepath.Clean(legacyRoot),
		"empty-agent scope Root must be the configured SyncDir /legacy-dir")
}
