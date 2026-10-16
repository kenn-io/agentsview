package remotesync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		Def: f.Definition(), Caps: f.Capabilities(), Config: cfg.Clone(), relevance: f.relevance,
	}
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
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
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
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
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

	fallbackEngine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
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

func TestDeferredDeltaRetainsJournalAndRequiredDataVersion(t *testing.T) {
	engineStats := syncpkg.SyncStats{Deferred: 1}
	assert.False(t, shouldPersistImportDataVersion(engineStats))
	assert.Equal(t, JournalProcessingFailures, deltaImportOutcome(engineStats),
		"deferred processing retains the journal")
	assert.NotEqual(t, JournalRetired, deltaImportOutcome(engineStats),
		"deferred processing must not advance the required data version")
}

func TestPreparedDeltaImportPoisonProjectionDisarmsAndRearms(t *testing.T) {
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
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
		engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
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
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
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

func TestPreparePendingPreservesCacheBoundariesAcrossParserUpgrades(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	const host = "remote"
	require.NoError(t, database.SetSyncState(t.Context(),
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
	unaffected := make(map[string]int64, 256)
	for i := range 256 {
		key := fmt.Sprintf("%s/unrelated/session-%03d.jsonl?agent=codex", remoteDir, i)
		cache[key] = int64(i)
		unaffected[key] = int64(i)
	}
	require.NoError(t, database.ReplaceRemoteSkippedFiles(t.Context(), host, cache))
	replaceCalls := 0
	type mutationCount struct{ deletes, upserts int }
	var mutations []mutationCount

	importer := Importer{
		Host: host, DB: database, Root: root,
		Targets: TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude:   {remoteDir},
			parser.AgentCodebuff: {"/srv/agent/codebuff"},
		}},
		replaceRemoteSkippedFiles: func(
			ctx context.Context, host string, cache map[string]int64,
		) error {
			replaceCalls++
			return database.ReplaceRemoteSkippedFiles(ctx, host, cache)
		},
		applyRemoteSkippedChanges: func(ctx context.Context,
			host string, deletes []string, upserts map[string]int64,
		) error {
			mutations = append(mutations, mutationCount{len(deletes), len(upserts)})
			return database.ApplyRemoteSkippedFileChanges(t.Context(), host, deletes, upserts)
		},
	}
	pending, err := importer.PreparePending(t.Context(), DeltaImportRequest{
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
	persisted, err := database.LoadRemoteSkippedFiles(t.Context(), host)
	require.NoError(t, err)
	assert.Len(t, persisted, 256)
	assert.NotContains(t, persisted, remoteDir+"/project/changed.jsonl")
	brokenPath := remappedRemotePath(root,
		"/srv/agent/codebuff/project/chats/broken/chat-messages.json")
	dbtest.WriteTestFile(t, brokenPath, []byte("{"))
	affectedKey := "/srv/agent/codebuff/project/chats/stale/chat-messages.json?agent=codebuff"
	changedInfo, err := os.Stat(changedPath)
	require.NoError(t, err)
	unaffected[remoteDir+"/project/changed.jsonl"] = changedInfo.ModTime().UnixNano()
	require.NoError(t, database.ApplyRemoteSkippedFileChanges(t.Context(), host,
		nil, map[string]int64{
			affectedKey:                          7,
			remoteDir + "/project/changed.jsonl": changedInfo.ModTime().UnixNano(),
		},
	))
	journal := MirrorChangeJournal{
		Version:             mirrorJournalVersion,
		RequiredDataVersion: db.CurrentDataVersion(),
		Entries:             pending.DisarmedJournal.Entries,
	}
	for attempt, wantVersion := range []int{0, db.CurrentDataVersion()} {
		pending, err = importer.PreparePending(t.Context(), DeltaImportRequest{
			Journal:                 journal,
			ScopedProviders:         []parser.AgentType{parser.AgentCodebuff},
			ResetAttemptCache:       journal.DataRebuildCacheVersion == 0,
			AttemptCacheDataVersion: db.CurrentDataVersion(),
		})
		require.NoError(t, err)
		journal = pending.DisarmedJournal
		_, err = pending.Execute(t.Context())
		if attempt == 0 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		version, err := database.RemoteImportDataVersion(t.Context(), host)
		require.NoError(t, err)
		assert.Equal(t, wantVersion, version)
		dbtest.WriteTestFile(t, brokenPath, []byte("[]"))
	}
	assert.Zero(t, replaceCalls, "a scoped reset must not rewrite the host cache")
	persisted, err = database.LoadRemoteSkippedFiles(t.Context(), host)
	require.NoError(t, err)
	assert.NotContains(t, persisted, affectedKey,
		"a scoped reset must drop the affected provider's skips")
	assert.Subset(t, persisted, unaffected, "unaffected provider skips survive scoped retries")
}

func TestPreparePendingPersistenceFailureAbortsBeforeProcessing(t *testing.T) {
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	require.NoError(t, database.ReplaceRemoteSkippedFiles(t.Context(),
		"remote", map[string]int64{"/srv/agent/sessions/pending.jsonl": 1},
	))
	sentinel := errors.New("prune persistence sentinel")
	_, err = (Importer{
		Host: "remote", DB: database, Root: t.TempDir(),
		Targets: TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/srv/agent/sessions"},
		}},
		replaceRemoteSkippedFiles: func(context.Context, string, map[string]int64) error {
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
		replaceRemoteSkippedFiles: func(context.Context, string, map[string]int64) error {
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
		replaceRemoteSkippedFiles: func(context.Context, string, map[string]int64) error {
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
	require.NoError(t, database.ReplaceRemoteSkippedFiles(t.Context(),
		"remote", map[string]int64{"/srv/agent/sessions/pending.jsonl": 1},
	))
	ctx, cancel := context.WithCancel(t.Context())
	persistCalls := 0
	pending, err := (Importer{
		Host: "remote", DB: database, Root: t.TempDir(),
		Targets: TargetSet{Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/srv/agent/sessions"},
		}},
		replaceRemoteSkippedFiles: func(context.Context, string, map[string]int64) error {
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
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
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

// TestImporterHermesRoutineImportSkipsUnchangedMemberDespiteFileForceFullParse
// proves a routine remote-mirror import's file-level ForceFullParse -- set
// through the real production journal entry flag (MirrorChangeEntry.
// ForceFullParse), not a test-only shortcut -- does not force-rewrite every
// Hermes session derived from that file. A freshly fetched state.db is
// marked ForceFullParse because the mirror layer can't incrementally diff a
// binary SQLite file, but that flag means "look at this file," not "every
// per-session member it fans out to must be rewritten." Before the fix, the
// flag propagated into every member's incForceReplace and defeated this
// provider's whole per-session freshness model on every routine import.
func TestImporterHermesRoutineImportSkipsUnchangedMemberDespiteFileForceFullParse(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			const remoteDir = "/srv/agent/sessions"
			localDir := remappedRemotePath(root, remoteDir)
			require.NoError(t, os.MkdirAll(localDir, 0o755))
			stateDB := filepath.Join(filepath.Dir(localDir), "state.db")
			writeHermesImportStateDB(t, stateDB)
			conn, err := sql.Open("sqlite3", stateDB)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions(id,source,model,started_at,message_count) VALUES('sibling','cli','model',1778767200,1); INSERT INTO messages(session_id,role,content,timestamp) VALUES('sibling','user','sibling message',1778767210)`)
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			targets := TargetSet{Dirs: map[parser.AgentType][]string{agent: {remoteDir}}}
			importer := Importer{Host: "remote", DB: database, Root: root, Targets: targets}
			seed, err := importer.ImportExtracted(t.Context(), targets, root)
			require.NoError(t, err)
			require.Equal(t, 2, seed.SessionsSynced)
			siblingID := "remote~" + string(agent) + ":sibling"
			before, err := database.GetSessionFull(t.Context(), siblingID)
			require.NoError(t, err)
			require.NotNil(t, before)

			path, err := mirrorRelativeLocalChangePath(root, stateDB)
			require.NoError(t, err)
			pending, err := importer.PreparePending(t.Context(), DeltaImportRequest{
				Journal: MirrorChangeJournal{
					Version: mirrorJournalVersion,
					Entries: []MirrorChangeEntry{{
						Path: path, InvalidateCache: true, ForceFullParse: true,
					}},
				},
			})
			require.NoError(t, err)
			stats, err := pending.Execute(t.Context())
			require.NoError(t, err)
			assert.Zero(t, stats.SessionsSynced,
				"a routine import's file-level ForceFullParse must not rewrite an unchanged member")
			after, err := database.GetSessionFull(t.Context(), siblingID)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

// TestImporterHermesStandaloneTranscriptEditReplacesStoredMessages covers
// the other side of the exemption above: a transcript with no state.db is
// the fetched file itself, so the journal's ForceFullParse must still
// replace its stored messages when an earlier message changed remotely.
func TestImporterHermesStandaloneTranscriptEditReplacesStoredMessages(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			const remoteDir = "/srv/agent/sessions"
			localDir := remappedRemotePath(root, remoteDir)
			require.NoError(t, os.MkdirAll(localDir, 0o755))
			transcript := filepath.Join(localDir, "standalone.jsonl")
			const reply = `{"role":"assistant","content":"first reply","timestamp":"2026-05-14T14:00:11Z"}` + "\n"
			require.NoError(t, os.WriteFile(transcript, []byte(
				`{"role":"user","content":"original prompt","timestamp":"2026-05-14T14:00:10Z"}`+"\n"+reply,
			), 0o600))
			targets := TargetSet{Dirs: map[parser.AgentType][]string{agent: {remoteDir}}}
			importer := Importer{Host: "remote", DB: database, Root: root, Targets: targets}
			seed, err := importer.ImportExtracted(t.Context(), targets, root)
			require.NoError(t, err)
			require.Equal(t, 1, seed.SessionsSynced)

			require.NoError(t, os.WriteFile(transcript, []byte(
				`{"role":"user","content":"edited prompt that is longer","timestamp":"2026-05-14T14:00:10Z"}`+"\n"+reply,
			), 0o600))
			path, err := mirrorRelativeLocalChangePath(root, transcript)
			require.NoError(t, err)
			pending, err := importer.PreparePending(t.Context(), DeltaImportRequest{
				Journal: MirrorChangeJournal{
					Version: mirrorJournalVersion,
					Entries: []MirrorChangeEntry{{
						Path: path, InvalidateCache: true, ForceFullParse: true,
					}},
				},
			})
			require.NoError(t, err)
			_, err = pending.Execute(t.Context())
			require.NoError(t, err)
			messages, err := database.GetAllMessages(t.Context(), "remote~"+string(agent)+":standalone")
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.Equal(t, "edited prompt that is longer", messages[0].Content)
		})
	}
}

func TestImporterHermesDatabaseDeltaBoundsSharedScans(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			const remoteDir = "/srv/agent/sessions"
			localDir := remappedRemotePath(root, remoteDir)
			require.NoError(t, os.MkdirAll(localDir, 0o755))
			stateDB := filepath.Join(filepath.Dir(localDir), "state.db")
			writeHermesImportStateDB(t, stateDB)
			conn, err := sql.Open("sqlite3", stateDB)
			require.NoError(t, err)
			const members = 2
			for i := range members {
				id := fmt.Sprintf("member-%03d", i)
				_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions(id,source,model,started_at,message_count) VALUES(?,'cli','model',1778767200,1); INSERT INTO messages(session_id,role,content,timestamp) VALUES(?,'user','sibling message',1778767210)`, id, id)
				require.NoError(t, err)
			}
			require.NoError(t, conn.Close())
			targets := TargetSet{Dirs: map[parser.AgentType][]string{agent: {remoteDir}}}
			importer := Importer{Host: "remote", DB: database, Root: root, Targets: targets}
			seed, err := importer.ImportExtracted(t.Context(), targets, root)
			require.NoError(t, err)
			require.Equal(t, members+1, seed.SessionsSynced)
			siblingID := "remote~" + string(agent) + ":member-000"
			before, err := database.GetSessionFull(t.Context(), siblingID)
			require.NoError(t, err)
			require.NotNil(t, before)
			conn, err = sql.Open("sqlite3", stateDB)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), "UPDATE messages SET content='changed database message' WHERE session_id='database-only'")
			require.NoError(t, err)
			require.NoError(t, conn.Close())
			path, err := mirrorRelativeLocalChangePath(root, stateDB)
			require.NoError(t, err)
			pending, err := importer.PreparePending(t.Context(), DeltaImportRequest{Journal: MirrorChangeJournal{Version: mirrorJournalVersion, Entries: []MirrorChangeEntry{{Path: path, InvalidateCache: true}}}})
			require.NoError(t, err)
			stats, err := pending.Execute(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, stats.SessionsSynced)
			after, err := database.GetSessionFull(t.Context(), siblingID)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			changed, err := database.GetSessionFull(t.Context(), "remote~"+string(agent)+":database-only")
			require.NoError(t, err)
			require.NotNil(t, changed)
			assert.Equal(t, new("changed database message"), changed.FirstMessage)
		})
	}
}

func TestImporterHermesIncompleteDiscoveryProcessesHealthyDelta(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			const sessionsDir = "/srv/agent/hermes/sessions"
			const otherDir = "/srv/agent/claude"
			localDir := remappedRemotePath(root, sessionsDir)
			require.NoError(t, os.MkdirAll(localDir, 0o755))
			stateDB := filepath.Join(filepath.Dir(localDir), "state.db")
			writeHermesImportStateDB(t, stateDB)
			transcript := filepath.Join(localDir, "database-only.jsonl")
			content := `{"role":"user","content":"database-only message","timestamp":"2026-05-14T14:00:10Z"}` + "\n"
			require.NoError(t, os.WriteFile(transcript, []byte(content), 0o600))
			otherPath := filepath.Join(remappedRemotePath(root, otherDir), "project", "other-session.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(otherPath), 0o755))
			require.NoError(t, os.WriteFile(otherPath, []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-08-15T10:00:00Z", "initial prompt", "other-session").String()), 0o600))
			targets := TargetSet{Dirs: map[parser.AgentType][]string{agent: {sessionsDir}, parser.AgentClaude: {otherDir}}}
			importer := Importer{Host: "remote", DB: database, Root: root, Targets: targets}
			_, err := importer.ImportExtracted(t.Context(), targets, root)
			require.NoError(t, err)
			otherBefore, err := database.GetSessionFull(t.Context(), "remote~other-session")
			require.NoError(t, err)
			require.NotNil(t, otherBefore)
			require.Equal(t, new("initial prompt"), otherBefore.FirstMessage)
			id := "remote~" + string(agent) + ":database-only"
			require.NoError(t, os.WriteFile(stateDB, []byte("not a SQLite database"), 0o600))
			require.NoError(t, os.WriteFile(transcript, []byte(content+`{"role":"assistant","content":"recovered transcript response","timestamp":"2026-05-14T14:00:11Z"}`+"\n"), 0o600))
			require.NoError(t, os.WriteFile(otherPath, []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-08-15T10:00:00Z", "healthy delta", "other-session").String()), 0o600))
			statePath, err := mirrorRelativeLocalChangePath(root, stateDB)
			require.NoError(t, err)
			otherRelative, err := mirrorRelativeLocalChangePath(root, otherPath)
			require.NoError(t, err)
			pending, err := importer.PreparePending(t.Context(), DeltaImportRequest{Journal: MirrorChangeJournal{Version: mirrorJournalVersion, Entries: []MirrorChangeEntry{{Path: otherRelative, InvalidateCache: true}, {Path: statePath, InvalidateCache: true}}}})
			require.NoError(t, err)
			stats, err := pending.Execute(t.Context())
			var incomplete parser.DiscoveryIncompleteError
			require.ErrorAs(t, err, &incomplete)
			assert.Equal(t, JournalProcessingFailures, stats.JournalOutcome)
			assert.Equal(t, 1, stats.Failed)
			assert.Equal(t, 2, stats.SessionsSynced)
			messages, err := database.GetAllMessages(t.Context(), id)
			require.NoError(t, err)
			assert.Len(t, messages, 2, "the recovered transcript's messages must survive the incomplete delta")
			other, err := database.GetSessionFull(t.Context(), "remote~other-session")
			require.NoError(t, err)
			require.NotNil(t, other)
			assert.Equal(t, new("healthy delta"), other.FirstMessage)
		})
	}
}

// TestImporterHermesSkipsUnchangedSessionFromNewExtractionRoot covers legacy
// HTTP imports, which unpack every archive into a fresh temporary folder. The
// same remote session arriving under a different local root must keep its
// fingerprint, so the second import skips it instead of rewriting it.
func TestImporterHermesSkipsUnchangedSessionFromNewExtractionRoot(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentHermes, parser.AgentAugureDesktop} {
		t.Run(string(agent), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			const remoteDir = "/srv/agent/sessions"
			mtime := time.Date(2026, 5, 14, 14, 0, 0, 0, time.UTC)
			seedState := filepath.Join(t.TempDir(), "state.db")
			writeHermesImportStateDB(t, seedState)
			stateBytes, err := os.ReadFile(seedState)
			require.NoError(t, err)
			transcript := []byte(`{"role":"user","content":"database-only message","timestamp":"2026-05-14T14:00:10Z"}` + "\n")
			targets := TargetSet{Dirs: map[parser.AgentType][]string{agent: {remoteDir}}}
			importFrom := func() int {
				root := t.TempDir()
				localDir := remappedRemotePath(root, remoteDir)
				require.NoError(t, os.MkdirAll(localDir, 0o755))
				files := map[string][]byte{
					filepath.Join(filepath.Dir(localDir), "state.db"): stateBytes,
					filepath.Join(localDir, "database-only.jsonl"):    transcript,
				}
				for path, content := range files {
					require.NoError(t, os.WriteFile(path, content, 0o600))
					require.NoError(t, os.Chtimes(path, mtime, mtime))
				}
				importer := Importer{Host: "remote", DB: database, Root: root, Targets: targets}
				stats, err := importer.ImportExtracted(t.Context(), targets, root)
				require.NoError(t, err)
				return stats.SessionsSynced
			}
			require.Equal(t, 1, importFrom())
			assert.Zero(t, importFrom(),
				"identical content from a new extraction root must not resync")
		})
	}
}
