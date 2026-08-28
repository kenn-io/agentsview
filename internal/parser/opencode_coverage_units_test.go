package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openCodeUnitProvider(t *testing.T, root string) Provider {
	t.Helper()
	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	return provider
}

// TestOpenCodeWatchPlanKeepsTheContainerOffTheRecursiveBudget is the reported
// failure: one recursive unit made the SQLite container and the storage
// archive draw on the same shared budget, so a root reached after that budget
// was spent registered no native watch at all and the live container went
// uncovered. A shallow unit never draws on it.
func TestOpenCodeWatchPlanKeepsTheContainerOffTheRecursiveBudget(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("db"), 0o600,
	))
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "storage", "session", "project"), 0o755,
	))

	plan, err := openCodeUnitProvider(t, root).WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 2)

	container := plan.Roots[0]
	assert.Equal(t, root, container.Path)
	assert.False(t, container.Recursive,
		"the container unit must never enter the recursive budget")
	assert.Equal(t, []string{"opencode*.db", "opencode*.db-wal"}, container.IncludeGlobs)

	storage := plan.Roots[1]
	assert.Equal(t, filepath.Join(root, "storage"), storage.Path)
	assert.True(t, storage.Recursive)
	assert.Equal(t, []string{"*.json"}, storage.IncludeGlobs)

	assert.NotEqual(t, container.DebounceKey, storage.DebounceKey,
		"units sharing a configured root need independent debounce keys")
}

// TestOpenCodeWatchPlanStaysWholeWhileStorageIsAbsent keeps the plan from
// naming a watch root that does not exist. A planned root the watcher cannot
// establish carries a polling obligation probed on that path, and a probe that
// can never be satisfied defers every other obligation on the same configured
// dir, leaving it with neither native coverage nor polling. The single
// recursive unit costs one native watch and still covers a storage tree
// created later.
func TestOpenCodeWatchPlanStaysWholeWhileStorageIsAbsent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("db"), 0o600,
	))
	require.NoDirExists(t, filepath.Join(root, "storage"))

	plan, err := openCodeUnitProvider(t, root).WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)
}

// TestOpenCodeWatchPlanNamesNoAbsentRootForAnUninstalledAgent is the cold-start
// layout: the configured dir itself does not exist yet, so the plan must leave
// exactly one probe, on the dir, rather than a deeper path that appears only
// afterwards.
func TestOpenCodeWatchPlanNamesNoAbsentRootForAnUninstalledAgent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-installed")
	require.NoDirExists(t, root)

	plan, err := openCodeUnitProvider(t, root).WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, root, plan.Roots[0].Path)
}

// TestOpenCodeWatchPlanKeepsASymlinkedRootRecursive preserves the daemon's
// symlink policy. It refuses to watch a recursive root through a symlink and
// gates the configured dir's reconciliation on the link target instead, and
// that check reads the unit's Recursive flag. A shallow container unit would
// slip past it while the storage unit walked the link's target anyway, leaving
// the dir recursively watched through the link with no availability probe.
func TestOpenCodeWatchPlanKeepsASymlinkedRootRecursive(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	require.NoError(t, os.MkdirAll(filepath.Join(target, "storage"), 0o755))
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	plan, err := openCodeUnitProvider(t, link).WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	assert.Equal(t, link, plan.Roots[0].Path)
	assert.True(t, plan.Roots[0].Recursive)

	// The single unit must still claim its own paths, or the symlinked root
	// would be watched and then classify nothing.
	set := newOpenCodeFormatSourceSet(
		[]string{link}, openCodeProviderSpecForAgent(AgentOpenCode), nil,
	)
	assert.True(t, set.unitScopeAllows(ChangedPathRequest{
		Path: filepath.Join(link, "storage", "session", "p", "s.json"), WatchRoot: link,
	}))
	assert.True(t, set.unitScopeAllows(ChangedPathRequest{
		Path: filepath.Join(link, "opencode.db-wal"), WatchRoot: link,
	}))
}

// TestOpenCodeNestedConfiguredRootsClaimEachPathOnce covers configured roots
// that nest. Every containing root's units would otherwise claim the path, so
// the deepest root owns it and the ancestor's units stand down.
func TestOpenCodeNestedConfiguredRootsClaimEachPathOnce(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "nested")
	require.NoError(t, os.MkdirAll(filepath.Join(inner, "storage"), 0o755))

	set := newOpenCodeFormatSourceSet(
		[]string{outer, inner}, openCodeProviderSpecForAgent(AgentOpenCode), nil,
	)
	innerWAL := filepath.Join(inner, "opencode.db-wal")

	assert.True(t, set.unitScopeAllows(ChangedPathRequest{
		Path: innerWAL, WatchRoot: inner,
	}))
	assert.False(t, set.unitScopeAllows(ChangedPathRequest{
		Path: innerWAL, WatchRoot: outer,
	}), "the outer container unit must not re-run the inner root's fan-out")

	innerSession := filepath.Join(inner, "storage", "session", "p", "s.json")
	assert.True(t, set.unitScopeAllows(ChangedPathRequest{
		Path: innerSession, WatchRoot: filepath.Join(inner, "storage"),
	}))
	assert.False(t, set.unitScopeAllows(ChangedPathRequest{
		Path: innerSession, WatchRoot: outer,
	}))
}

// TestOpenCodeChangedPathClaimedByExactlyOneUnit covers the fan-out the split
// would otherwise double. The engine dispatches every event once per emitted
// watch root, so a WAL write reaching both units would run the whole
// container's session listing twice for one change. Storage paths are not
// scoped this way; see TestOpenCodeStorageSessionSurvivesAStaleDispatchSet.
func TestOpenCodeChangedPathClaimedByExactlyOneUnit(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	seedHybridSQLiteDB(t, dbPath, "ses_container")
	storage := filepath.Join(root, "storage")
	sessionDir := filepath.Join(storage, "session", "project")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	sessionPath := filepath.Join(sessionDir, "ses_unit.json")
	require.NoError(t, os.WriteFile(sessionPath, []byte(`{"id":"ses_unit"}`), 0o600))

	provider := openCodeUnitProvider(t, root)
	ctx := context.Background()

	walPath := filepath.Join(root, "opencode.db-wal")
	require.NoError(t, os.WriteFile(walPath, make([]byte, 4096), 0o600))

	fromStorageUnit, err := provider.SourcesForChangedPath(ctx, ChangedPathRequest{
		Path: walPath, EventKind: "write", WatchRoot: storage,
	})
	require.NoError(t, err)
	assert.Empty(t, fromStorageUnit,
		"the storage unit must not claim a change outside its own subtree")

	fromContainerUnit, err := provider.SourcesForChangedPath(ctx, ChangedPathRequest{
		Path: walPath, EventKind: "write", WatchRoot: root,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, fromContainerUnit,
		"the container unit owns its database and WAL")

	sessionFromStorage, err := provider.SourcesForChangedPath(ctx, ChangedPathRequest{
		Path: sessionPath, EventKind: "write", WatchRoot: storage,
	})
	require.NoError(t, err)
	require.Len(t, sessionFromStorage, 1)
	assert.Equal(t, sessionPath, sessionFromStorage[0].DisplayPath)
}

// TestOpenCodeStorageSessionSurvivesAStaleDispatchSet is the skew case. The
// engine resolves each provider's watch roots once and reuses that set for the
// life of the process, so a storage tree created afterwards is still
// dispatched only against the configured root. Deferring storage paths to the
// storage unit would leave them claimed by no unit at all until the next
// daemon start.
func TestOpenCodeStorageSessionSurvivesAStaleDispatchSet(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("db"), 0o600,
	))
	provider := openCodeUnitProvider(t, root)

	// The dispatch set the engine caches: storage does not exist yet, so the
	// plan is the single recursive unit at the configured root.
	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, plan.Roots, 1)
	cachedWatchRoot := plan.Roots[0].Path

	sessionDir := filepath.Join(root, "storage", "session", "project")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	sessionPath := filepath.Join(sessionDir, "ses_skew.json")
	require.NoError(t, os.WriteFile(sessionPath, []byte(`{"id":"ses_skew"}`), 0o600))

	refreshed, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	require.Len(t, refreshed.Roots, 2, "the live plan has split by now")

	sources, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{
			Path: sessionPath, EventKind: "write", WatchRoot: cachedWatchRoot,
		},
	)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, sessionPath, sources[0].DisplayPath)
}

// TestOpenCodeUnscopedChangedPathStaysUnfiltered keeps callers that do not
// dispatch per watch root working. Reconciliation rehydration and poll-driven
// classification both send an empty WatchRoot, and gating them would drop
// every source they resolve.
func TestOpenCodeUnscopedChangedPathStaysUnfiltered(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "opencode.db"), []byte("db"), 0o600,
	))
	sessionDir := filepath.Join(root, "storage", "session", "project")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	sessionPath := filepath.Join(sessionDir, "ses_unscoped.json")
	require.NoError(t, os.WriteFile(
		sessionPath, []byte(`{"id":"ses_unscoped"}`), 0o600,
	))

	sources, err := openCodeUnitProvider(t, root).SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sessionPath, EventKind: "write"},
	)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, sessionPath, sources[0].DisplayPath)
}

// TestOpenCodeVirtualMemberScopesToItsContainerUnit pins the virtual spelling.
// A member path is <dbPath>#<sessionID>, which lies under no watch root as
// written, so the scope check must resolve it to the physical container first
// or the container unit would drop every member event it owns.
func TestOpenCodeVirtualMemberScopesToItsContainerUnit(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("db"), 0o600))

	set := newOpenCodeFormatSourceSet(
		[]string{root}, openCodeProviderSpecForAgent(AgentOpenCode), nil,
	)
	virtual := OpenCodeSQLiteVirtualPath(dbPath, "ses_virtual")

	assert.True(t, set.unitScopeAllows(ChangedPathRequest{
		Path: virtual, WatchRoot: root,
	}))
	assert.False(t, set.unitScopeAllows(ChangedPathRequest{
		Path: virtual, WatchRoot: filepath.Join(root, "storage"),
	}))
}
