package remotesync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

type deltaPlanFactory struct {
	agent     parser.AgentType
	relevance parser.ChangedPathRelevance
}

func (f deltaPlanFactory) Definition() parser.AgentDef {
	return parser.AgentDef{Type: f.agent, DisplayName: string(f.agent)}
}

func (f deltaPlanFactory) Capabilities() parser.Capabilities {
	return parser.Capabilities{Source: parser.SourceCapabilities{
		ClassifyChangedPath:  parser.CapabilitySupported,
		ChangedPathRelevance: parser.CapabilitySupported,
	}}
}

func (f deltaPlanFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	return &deltaPlanProvider{
		Def: f.Definition(), Caps: f.Capabilities(), Config: cfg.Clone(), relevance: f.relevance}
}

type deltaPlanProvider struct {
	parser.ProviderBase
	relevance parser.ChangedPathRelevance
}

func (p *deltaPlanProvider) WatchPlan(context.Context) (parser.WatchPlan, error) {
	return parser.WatchPlan{Roots: []parser.WatchRoot{{Path: p.Config.Roots[0]}}}, nil
}

func (p *deltaPlanProvider) ChangedPathRelevance(
	context.Context, parser.ChangedPathRequest,
) (parser.ChangedPathRelevance, error) {
	return p.relevance, nil
}

func (p *deltaPlanProvider) SourcesForChangedPath(
	_ context.Context, req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	return []parser.SourceRef{{
		Provider: p.Definition().Type, Key: req.Path,
		DisplayPath: req.Path, FingerprintKey: req.Path,
	}}, nil
}

func (p *deltaPlanProvider) Parse(context.Context, parser.ParseRequest) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}

func TestRemotePathMapStoredResolver(t *testing.T) {
	root := t.TempDir()
	remoteDir := "/srv/agent/sessions"
	localDir := remappedRemotePath(root, remoteDir)
	paths := remotePathMap{
		host: "remote", root: root,
		remoteDirs: []string{remoteDir}, localDirs: []string{localDir},
	}
	resolve := paths.storedPathResolver()

	local, ok := resolve("remote:/srv/agent/sessions/project/session.jsonl")
	require.True(t, ok)
	assert.Equal(t, filepath.Join(localDir, "project", "session.jsonl"), local)
	assert.Equal(t, "remote:/srv/agent/sessions/project/session.jsonl", paths.pathRewriter()(local))

	_, ok = resolve("other:/srv/agent/sessions/project/session.jsonl")
	assert.False(t, ok)
	_, ok = resolve("remote:/srv/other/session.jsonl")
	assert.False(t, ok)
	_, ok = resolve("remote:/srv/agent/sessions/../escape.jsonl")
	assert.False(t, ok)
}

func TestPruneRemoteSkipCacheHostWideAndDisarmed(t *testing.T) {
	root := t.TempDir()
	remoteDir := "/srv/agent/sessions"
	layout := importLayout{
		engineDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {remappedRemotePath(root, remoteDir)},
		},
		paths: remotePathMap{
			host: "remote", root: root,
			remoteDirs: []string{remoteDir},
			localDirs:  []string{remappedRemotePath(root, remoteDir)},
		},
	}
	cache := map[string]int64{
		remoteDir + "/one.jsonl":               1,
		remoteDir + "/two.jsonl?agent=claude":  2,
		remoteDir + "/three.jsonl?agent=codex": 3,
	}

	pruned, stats := pruneRemoteSkipCache(cache, layout, syncpkg.ChangedPathPlan{}, MirrorChangeJournal{
		Version: mirrorJournalVersion, InvalidateAll: true,
	})
	assert.Empty(t, pruned)
	assert.True(t, stats.HostWideScope)
	assert.Equal(t, 3, stats.HostWide)

	pruned, stats = pruneRemoteSkipCache(cache, layout, syncpkg.ChangedPathPlan{}, MirrorChangeJournal{
		Version: mirrorJournalVersion,
	})
	assert.Equal(t, cache, pruned)
	assert.Zero(t, stats.Total())
}

func TestPruneRemoteSkipCacheExactAndFallbackFamilies(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	root := t.TempDir()
	remoteDir := "/srv/agent/sessions"
	localDir := remappedRemotePath(root, remoteDir)
	agent := parser.AgentType("delta-provider")
	layout := importLayout{
		engineDirs: map[parser.AgentType][]string{agent: {localDir}},
		paths: remotePathMap{
			host: "remote", root: root,
			remoteDirs: []string{remoteDir}, localDirs: []string{localDir},
		},
	}
	changed := filepath.Join(localDir, "changed.jsonl")
	journalPath, err := mirrorRelativeLocalChangePath(root, changed)
	require.NoError(t, err)
	journal := MirrorChangeJournal{Version: mirrorJournalVersion, Entries: []MirrorChangeEntry{{
		Path: journalPath, InvalidateCache: true,
	}}}
	cache := map[string]int64{
		remoteDir + "/changed.jsonl":                                    1,
		remoteDir + "/changed.jsonl?agent=delta-provider":               2,
		remoteDir + "/changed.jsonl?agent=delta-provider?source_hash=x": 3,
		remoteDir + "/changed.jsonl?agent=other":                        4,
		remoteDir + "/unchanged.jsonl?agent=delta-provider":             5,
	}
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {localDir}},
		Machine:   "remote", Ephemeral: true,
		ProviderFactories: []parser.ProviderFactory{deltaPlanFactory{
			agent: agent, relevance: parser.ChangedPathDataBearing,
		}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	plan, err := engine.PlanChangedPathsContext(t.Context(), []string{changed})
	require.NoError(t, err)

	pruned, stats := pruneRemoteSkipCache(cache, layout, plan, journal)
	assert.Equal(t, 1, stats.ExactScopes)
	assert.Zero(t, stats.ProviderScopes)
	assert.Equal(t, 3, stats.Exact)
	assert.Zero(t, stats.Provider)
	assert.Equal(t, map[string]int64{
		remoteDir + "/changed.jsonl?agent=other":            4,
		remoteDir + "/unchanged.jsonl?agent=delta-provider": 5,
	}, pruned)

	fallbackEngine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {localDir}},
		Machine:   "remote", Ephemeral: true,
		ProviderFactories: []parser.ProviderFactory{deltaPlanFactory{
			agent: agent, relevance: parser.ChangedPathUnclassified,
		}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(fallbackEngine.Close)
	fallbackPlan, err := fallbackEngine.PlanChangedPathsContext(t.Context(), []string{changed})
	require.NoError(t, err)
	pruned, stats = pruneRemoteSkipCache(cache, layout, fallbackPlan, journal)
	assert.Zero(t, stats.ExactScopes)
	assert.Equal(t, 1, stats.ProviderScopes)
	assert.Equal(t, 4, stats.Provider)
	assert.Equal(t, map[string]int64{
		remoteDir + "/changed.jsonl?agent=other": 4,
	}, pruned)

	disarmed := disarmMirrorChanges(journal)
	pruned, stats = pruneRemoteSkipCache(cache, layout, plan, disarmed)
	assert.Equal(t, cache, pruned)
	assert.Zero(t, stats.Total())
}

func TestJournalOutcomeClosedSet(t *testing.T) {
	want := []JournalOutcome{
		JournalRetired,
		JournalAbortedBeforeProcessing,
		JournalProcessingFailures,
		JournalCachePersistFailed,
		JournalRetirementFailed,
		JournalCancelled,
		JournalPendingSwap,
	}
	for _, outcome := range want {
		assert.True(t, outcome.Valid(), "outcome %q must be reportable", outcome)
	}
	assert.False(t, JournalOutcome("retained(invented)").Valid())
}

func TestPreparedDeltaImportPoisonProjectionDisarmsAndRearms(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	root := t.TempDir()
	remoteDir := "/srv/agent/sessions"
	localDir := remappedRemotePath(root, remoteDir)
	agent := parser.AgentType("delta-provider")
	changed := filepath.Join(localDir, "poison.jsonl")
	journalPath, err := mirrorRelativeLocalChangePath(root, changed)
	require.NoError(t, err)
	journal := MirrorChangeJournal{Version: mirrorJournalVersion, Entries: []MirrorChangeEntry{{
		Path: journalPath, InvalidateCache: true,
	}}}
	layout := importLayout{
		engineDirs: map[parser.AgentType][]string{agent: {localDir}},
		paths: remotePathMap{
			host: "remote", root: root,
			remoteDirs: []string{remoteDir}, localDirs: []string{localDir},
		},
	}
	buildPlan := func(relevance parser.ChangedPathRelevance) syncpkg.ChangedPathPlan {
		engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
			AgentDirs: map[parser.AgentType][]string{agent: {localDir}},
			Machine:   "remote", Ephemeral: true,
			ProviderFactories: []parser.ProviderFactory{deltaPlanFactory{
				agent: agent, relevance: relevance,
			}},
			ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
				agent: parser.ProviderMigrationProviderAuthoritative,
			},
		})
		t.Cleanup(engine.Close)
		plan, planErr := engine.PlanChangedPathsContext(t.Context(), []string{changed})
		require.NoError(t, planErr)
		return plan
	}

	for _, tc := range []struct {
		name      string
		relevance parser.ChangedPathRelevance
		result    syncpkg.ChangedPathSyncResult
	}{
		{
			name: "exact", relevance: parser.ChangedPathDataBearing,
			result: syncpkg.ChangedPathSyncResult{CachedSourceKeys: map[string]struct{}{
				string(agent) + "\x00" + changed: {},
			}},
		},
		{
			name: "fallback", relevance: parser.ChangedPathUnclassified,
			result: syncpkg.ChangedPathSyncResult{
				CachedFallbackProviders: map[parser.AgentType]int{agent: 1},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := buildPlan(tc.relevance)
			cache := map[string]int64{
				remoteDir + "/poison.jsonl?agent=delta-provider": 42,
			}
			_, armedStats := pruneRemoteSkipCache(cache, layout, plan, journal)
			assert.Equal(t, 1, armedStats.Total())

			disarmed := disarmMirrorChanges(journal)
			preserved, replayStats := pruneRemoteSkipCache(cache, layout, plan, disarmed)
			assert.Equal(t, cache, preserved)
			assert.Zero(t, replayStats.Total())
			assert.Equal(t, 1, countDisarmedCachedSuppressions(plan, disarmed, tc.result))

			rearmed, _, mergeErr := mergeMirrorChanges(disarmed, []string{journalPath})
			require.NoError(t, mergeErr)
			_, rearmedStats := pruneRemoteSkipCache(cache, layout, plan, rearmed)
			assert.Equal(t, 1, rearmedStats.Total())
			assert.Zero(t, countDisarmedCachedSuppressions(plan, rearmed, tc.result))
		})
	}
}

func TestPreparedDeltaImportPrepareDoesNotProcess(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	root := t.TempDir()
	remoteDir := "/srv/agent/sessions"
	pending, err := (Importer{
		Host: "remote", DB: database,
		Targets: TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {remoteDir},
		}},
		Root: root,
	}).PreparePending(t.Context(), DeltaImportRequest{
		Journal: MirrorChangeJournal{Version: mirrorJournalVersion},
	})
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Zero(t, pending.Stats.SessionsSynced)
	assert.Equal(t, JournalAbortedBeforeProcessing, pending.Stats.JournalOutcome)
}

func TestPreparePendingExactChangeLoadsOnlyRelevantSkipCache(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	const host = "remote"
	require.NoError(t, database.SetSyncState(
		visualStudioCopilotRemoteSkipMigrationKey(host), "done",
	))
	root := t.TempDir()
	remoteDir := "/srv/agent/sessions"
	localDir := remappedRemotePath(root, remoteDir)
	changedPath := filepath.Join(localDir, "project", "changed.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(changedPath), 0o755))
	require.NoError(t, os.WriteFile(changedPath, []byte(
		testjsonl.NewSessionBuilder().AddClaudeUser(
			"2026-08-15T10:00:00Z", "bounded cache fixture",
		).String(),
	), 0o600))
	journalPath, err := mirrorRelativeLocalChangePath(root, changedPath)
	require.NoError(t, err)
	cache := map[string]int64{
		remoteDir + "/project/changed.jsonl": 42,
	}
	for i := range 256 {
		cache[fmt.Sprintf("%s/unrelated/session-%03d.jsonl", remoteDir, i)] = int64(i)
	}
	require.NoError(t, database.ReplaceRemoteSkippedFiles(host, cache))
	replaceCalls := 0
	type mutationCount struct{ deletes, upserts int }
	var mutations []mutationCount

	pending, err := (Importer{
		Host: host, DB: database, Root: root,
		Targets: TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {remoteDir},
		}},
		replaceRemoteSkippedFiles: func(string, map[string]int64) error {
			replaceCalls++
			return nil
		},
		applyRemoteSkippedChanges: func(
			host string, deletes []string, upserts map[string]int64,
		) error {
			mutations = append(mutations, mutationCount{len(deletes), len(upserts)})
			return database.ApplyRemoteSkippedFileChanges(host, deletes, upserts)
		},
	}).PreparePending(t.Context(), DeltaImportRequest{
		Journal: MirrorChangeJournal{
			Version: mirrorJournalVersion,
			Entries: []MirrorChangeEntry{{Path: journalPath}},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Len(t, pending.cache, 1,
		"unrelated host cache entries must not enter a one-source import")
	assert.Zero(t, replaceCalls,
		"a disarmed exact import must not rewrite unchanged cache rows")

	stats, err := pending.Execute(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	assert.Equal(t, []mutationCount{{deletes: 1}}, mutations,
		"processing must persist only the relevant cache-family change")
	persisted, err := database.LoadRemoteSkippedFiles(host)
	require.NoError(t, err)
	assert.Len(t, persisted, 256)
	assert.NotContains(t, persisted, remoteDir+"/project/changed.jsonl")
}

func TestPreparePendingPersistenceFailureAbortsBeforeProcessing(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.NoError(t, database.ReplaceRemoteSkippedFiles(
		"remote", map[string]int64{"/srv/agent/sessions/pending.jsonl": 1},
	))
	sentinel := errors.New("prune persistence sentinel")
	_, err = (Importer{
		Host: "remote", DB: database, Root: t.TempDir(),
		Targets: TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/srv/agent/sessions"},
		}},
		replaceRemoteSkippedFiles: func(string, map[string]int64) error {
			return sentinel
		},
	}).PreparePending(t.Context(), DeltaImportRequest{
		Journal: MirrorChangeJournal{
			Version: mirrorJournalVersion, FullImport: true,
			FullImportReason: FullImportJournalOverflow, InvalidateAll: true,
		},
	})
	require.ErrorIs(t, err, sentinel)
}

func TestPreparePendingRejectsUnknownReasonBeforePersistence(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	persistCalls := 0
	_, err := (Importer{
		Host: "remote", DB: database, Root: t.TempDir(),
		Targets: TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/srv/agent/sessions"},
		}},
		replaceRemoteSkippedFiles: func(string, map[string]int64) error {
			persistCalls++
			return nil
		},
	}).PreparePending(t.Context(), DeltaImportRequest{
		Journal:    MirrorChangeJournal{Version: mirrorJournalVersion},
		FullReason: FullImportReason("invented"),
	})
	require.ErrorContains(t, err, "invalid full import reason")
	assert.Zero(t, persistCalls)
}

func TestPreparePendingCancellationDoesNotPruneOrDisarm(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	persistCalls := 0
	_, err := (Importer{
		Host: "remote", DB: database, Root: t.TempDir(),
		Targets: TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/srv/agent/sessions"},
		}},
		replaceRemoteSkippedFiles: func(string, map[string]int64) error {
			persistCalls++
			return nil
		},
	}).PreparePending(ctx, DeltaImportRequest{
		Journal: MirrorChangeJournal{
			Version: mirrorJournalVersion, FullImport: true,
			FullImportReason: FullImportJournalOverflow, InvalidateAll: true,
		},
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, persistCalls)
}

func TestPreparePendingCancellationAfterPrunePersistenceDoesNotReturnFlip(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.ReplaceRemoteSkippedFiles(
		"remote", map[string]int64{"/srv/agent/sessions/pending.jsonl": 1},
	))
	ctx, cancel := context.WithCancel(t.Context())
	persistCalls := 0
	pending, err := (Importer{
		Host: "remote", DB: database, Root: t.TempDir(),
		Targets: TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/srv/agent/sessions"},
		}},
		replaceRemoteSkippedFiles: func(string, map[string]int64) error {
			persistCalls++
			cancel()
			return nil
		},
	}).PreparePending(ctx, DeltaImportRequest{
		Journal: MirrorChangeJournal{
			Version: mirrorJournalVersion, FullImport: true,
			FullImportReason: FullImportJournalOverflow, InvalidateAll: true,
		},
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, pending,
		"the caller must not receive a disarmed journal after cancellation")
	assert.Equal(t, 1, persistCalls)
}

func TestPreparedDeltaImportFinalCacheFailureRetainsJournal(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	sentinel := errors.New("final cache sentinel")
	pending, err := (Importer{
		Host: "remote", DB: database, Root: t.TempDir(),
		Targets: TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/srv/agent/sessions"},
		}},
		saveSkipCache: func(*db.DB, *syncpkg.Engine, remotePathMap) error {
			return sentinel
		},
	}).PreparePending(t.Context(), DeltaImportRequest{
		Journal: MirrorChangeJournal{Version: mirrorJournalVersion},
	})
	require.NoError(t, err)
	stats, err := pending.Execute(t.Context())
	require.ErrorIs(t, err, sentinel)
	assert.Equal(t, JournalCachePersistFailed, stats.JournalOutcome)
}
