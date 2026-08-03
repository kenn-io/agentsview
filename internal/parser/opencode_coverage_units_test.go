package parser

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var openCodeFamilyWatchUnitAgents = []struct {
	agent         AgentType
	dbName        string
	sessionSubdir string
}{
	{agent: AgentOpenCode, dbName: "opencode.db", sessionSubdir: "session"},
	{agent: AgentKilo, dbName: "kilo.db", sessionSubdir: "session"},
	{agent: AgentMiMoCode, dbName: "mimocode.db", sessionSubdir: "session_diff"},
	{agent: AgentIcodemate, dbName: "icodemate.db", sessionSubdir: "session_diff"},
}

func openCodeFamilyContainerUnit(
	agent AgentType, dbName, root string,
) WatchRoot {
	return WatchRoot{
		Path:         root,
		Recursive:    false,
		IncludeGlobs: []string{dbName, dbName + "-wal"},
		DebounceKey:  string(agent) + ":container:" + root,
	}
}

func openCodeFamilyStorageUnit(agent AgentType, root string) WatchRoot {
	return WatchRoot{
		Path:         filepath.Join(root, "storage"),
		Recursive:    true,
		Optional:     true,
		IncludeGlobs: []string{"*.json"},
		DebounceKey:  string(agent) + ":storage:" + root,
	}
}

// TestOpenCodeWatchUnits pins the two-unit emission shape
// for every OpenCode-family agent across resolved mode, database presence,
// and root existence: both the shallow container unit and the recursive
// storage unit are emitted from the configured plan, including a missing
// storage directory.
func TestOpenCodeWatchUnits(t *testing.T) {
	for _, agentCase := range openCodeFamilyWatchUnitAgents {
		t.Run(string(agentCase.agent), func(t *testing.T) {
			_, ok := ProviderFactoryByType(agentCase.agent)
			require.True(t, ok,
				"the provider factory must exist so the typed WatchPlan, "+
					"not the legacy WatchRootsFunc fallback, owns the plan")

			for _, tc := range []struct {
				name        string
				setup       func(t *testing.T) string
				withStorage bool
			}{
				{
					name: "missing root",
					setup: func(t *testing.T) string {
						return filepath.Join(t.TempDir(), "absent")
					},
				},
				{
					name: "mode none",
					setup: func(t *testing.T) string {
						return t.TempDir()
					},
				},
				{
					name: "sqlite",
					setup: func(t *testing.T) string {
						root := t.TempDir()
						writeTestFileHelper(t,
							filepath.Join(root, agentCase.dbName))
						return root
					},
				},
				{
					name: "storage",
					setup: func(t *testing.T) string {
						root := t.TempDir()
						require.NoError(t, os.MkdirAll(filepath.Join(
							root, "storage", agentCase.sessionSubdir,
						), 0o755))
						return root
					},
					withStorage: true,
				},
				{
					name: "hybrid",
					setup: func(t *testing.T) string {
						root := t.TempDir()
						require.NoError(t, os.MkdirAll(filepath.Join(
							root, "storage", agentCase.sessionSubdir,
						), 0o755))
						writeTestFileHelper(t,
							filepath.Join(root, agentCase.dbName))
						return root
					},
					withStorage: true,
				},
				{
					// The session subdirectory is created lazily, so a root
					// can carry storage/ with nothing under it yet. The
					// session tree that appears there later is a grandchild
					// of the configured root, which the shallow unit cannot
					// see, so this shape must still get recursive coverage.
					name: "storage tree before first session",
					setup: func(t *testing.T) string {
						root := t.TempDir()
						require.NoError(t, os.MkdirAll(
							filepath.Join(root, "storage"), 0o755))
						return root
					},
					withStorage: true,
				},
				{
					name: "sqlite with empty storage tree",
					setup: func(t *testing.T) string {
						root := t.TempDir()
						require.NoError(t, os.MkdirAll(
							filepath.Join(root, "storage"), 0o755))
						writeTestFileHelper(t,
							filepath.Join(root, agentCase.dbName))
						return root
					},
					withStorage: true,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					root := tc.setup(t)
					provider, ok := NewProvider(agentCase.agent, ProviderConfig{
						Roots: []string{root},
					})
					require.True(t, ok)

					plan, err := provider.WatchPlan(context.Background())
					require.NoError(t, err)

					want := []WatchRoot{openCodeFamilyContainerUnit(
						agentCase.agent, agentCase.dbName, root,
					)}
					want = append(want, openCodeFamilyStorageUnit(
						agentCase.agent, root,
					))
					assert.Equal(t, want, plan.Roots)
				})
			}
		})
	}
}

// TestOpenCodeSourcesForChangedPathUnitScope pins the unit-scope guard: with
// two units per hybrid root, exactly one unit claims each changed path, so a
// WAL event never runs the SQLite fan-out once per unit, and an empty
// WatchRoot keeps unscoped behavior for callers that do not dispatch per
// watch root.
func TestOpenCodeSourcesForChangedPathUnitScope(t *testing.T) {
	fixture := openCodeSQLiteProviderReadFixture(t)
	root := fixture.Root
	storageUnit := filepath.Join(root, "storage")
	storageSessionPath := writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_hybrid_store", "hybrid-app", "Hybrid",
	)
	walPath := fixture.DBPath + "-wal"
	require.NoError(t, os.WriteFile(
		walPath, bytes.Repeat([]byte{0x1}, 64), 0o644,
	), "a WAL larger than its header carries frames")

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)
	changed := func(path, watchRoot string) []SourceRef {
		t.Helper()
		sources, err := provider.SourcesForChangedPath(
			context.Background(),
			ChangedPathRequest{
				Path: path, EventKind: "write", WatchRoot: watchRoot,
			},
		)
		require.NoError(t, err)
		return sources
	}

	t.Run("wal claimed by container unit only", func(t *testing.T) {
		assert.Empty(t, changed(walPath, storageUnit),
			"a WAL event against the storage unit must not fan out")
		containerSources := changed(walPath, root)
		require.Len(t, containerSources, len(fixture.SessionIDs),
			"the container unit owns the SQLite fan-out")
		assert.Equal(t, containerSources, changed(walPath, ""),
			"an empty WatchRoot must behave exactly like base")
	})

	t.Run("storage session claimed by storage unit only", func(t *testing.T) {
		storageSources := changed(storageSessionPath, storageUnit)
		require.Len(t, storageSources, 1)
		assert.Equal(t, storageSessionPath, storageSources[0].DisplayPath)
		assert.Empty(t, changed(storageSessionPath, root),
			"the container unit must not double-claim storage paths")
		assert.Equal(t, storageSources, changed(storageSessionPath, ""),
			"an empty WatchRoot must behave exactly like base")
	})

	t.Run("virtual path scoped by its database path", func(t *testing.T) {
		assert.Empty(t, changed(fixture.SQLiteVirtualPath, storageUnit),
			"a virtual source's database is outside the storage unit")
		containerSources := changed(fixture.SQLiteVirtualPath, root)
		require.Len(t, containerSources, 1)
		assert.Equal(t, containerSources,
			changed(fixture.SQLiteVirtualPath, ""),
			"an empty WatchRoot must behave exactly like base")
	})
}

// TestOpenCodeWatchUnitsCoverAllDiscoveredSources maps every discovered
// source's physical path onto an emitted unit: virtual SQLite sources resolve
// to a database that is a direct child of the shallow container unit, and
// storage sources sit at or under the recursive storage unit, so the split
// plan loses no coverage relative to the single recursive root.
func TestOpenCodeWatchUnitsCoverAllDiscoveredSources(t *testing.T) {
	fixture := openCodeSQLiteProviderReadFixture(t)
	root := fixture.Root
	writeOpenCodeProviderStorageSession(
		t, root, "session", "ses_cover_store", "cover-app", "Coverage",
	)

	provider, ok := NewProvider(AgentOpenCode, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	var container, storage WatchRoot
	for _, unit := range plan.Roots {
		if unit.Recursive {
			storage = unit
		} else {
			container = unit
		}
	}
	require.NotEmpty(t, container.Path, "hybrid plan must emit a container unit")
	require.NotEmpty(t, storage.Path, "hybrid plan must emit a storage unit")

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, discovered)
	for _, source := range discovered {
		physical := source.DisplayPath
		if dbPath, _, virtual := parseOpenCodeFormatVirtualPath(
			"opencode.db", physical,
		); virtual {
			assert.Equal(t, container.Path, filepath.Dir(dbPath),
				"virtual source %s must resolve to a direct child of the "+
					"container unit", physical)
			continue
		}
		_, under := relUnder(storage.Path, physical)
		assert.True(t, under,
			"storage source %s must live under the storage unit", physical)
	}
}

// TestNonOpenCodeWatchPlanUnchanged pins another provider family's plan value
// so the coverage-unit split provably stays inside the OpenCode family.
func TestNonOpenCodeWatchPlanUnchanged(t *testing.T) {
	root := t.TempDir()
	provider, ok := NewProvider(AgentGemini, ProviderConfig{
		Roots: []string{root},
	})
	require.True(t, ok)

	plan, err := provider.WatchPlan(context.Background())
	require.NoError(t, err)
	tmp := filepath.Join(root, "tmp")
	assert.Equal(t, []WatchRoot{
		{
			Path:         tmp,
			Recursive:    true,
			IncludeGlobs: []string{"session-*.json", "session-*.jsonl"},
			DebounceKey:  string(AgentGemini) + ":tmp:" + tmp,
		},
		{
			Path:         root,
			Recursive:    false,
			IncludeGlobs: []string{"projects.json", "trustedFolders.json"},
			DebounceKey:  string(AgentGemini) + ":projects:" + root,
		},
	}, plan.Roots)
}

func writeTestFileHelper(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("stub"), 0o644))
}
