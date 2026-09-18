package sync

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSyncChangedPathPlanDoesNotTombstonePendingDeletion(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	deletedPath := filepath.Join(t.TempDir(), "deleted.jsonl")
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "remote:retained", Agent: string(parser.AgentCowork),
		Project: "fixture", Machine: "remote", FilePath: &deletedPath,
	}))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "remote", Ephemeral: true})
	t.Cleanup(engine.Close)
	plan := ChangedPathPlan{attribution: map[string]changedPathAttribution{
		deletedPath: {provenIrrelevant: true},
	}}

	result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.NoError(t, err)
	assert.Zero(t, result.FilesDiscovered)
	retained, err := database.GetSession(t.Context(), "remote:retained")
	require.NoError(t, err)
	require.NotNil(t, retained)
	assert.Nil(t, retained.DeletedAt)
}

func TestSyncChangedPathPlanReportsOnlyMtimeCacheSuppression(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "cached.jsonl")
	fingerprint := parser.SourceFingerprint{Key: path, Size: 7, MTimeNS: 42}
	source := parser.SourceRef{
		Provider: parser.AgentCowork, Key: path,
		DisplayPath: path, FingerprintKey: path,
	}
	provider := newProcessFixtureProvider(source, fingerprint, parser.ParseOutcome{})
	provider.Caps.Sync.SkipCacheFreshWithoutStoredRow = true
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCowork: {root}},
		Machine:   "remote", Ephemeral: true,
		ProviderFactories: []parser.ProviderFactory{processFixtureFactory{provider: provider}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	cacheKey := providerAgentSkipCacheKey(path, parser.AgentCowork)
	engine.skipCache[cacheKey] = fingerprint.MTimeNS
	file := parser.DiscoveredFile{
		Path: path, Agent: parser.AgentCowork,
		ProviderSource: &source, ProviderProcess: true,
	}
	plan := ChangedPathPlan{
		Files: []parser.DiscoveredFile{file},
		attribution: map[string]changedPathAttribution{
			path: {files: []parser.DiscoveredFile{file}},
		},
	}

	result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.FilesDiscovered)
	assert.Equal(t, 1, result.FilesProcessed)
	assert.Equal(t, 1, result.Stats.Skipped)
	assert.Equal(t, map[string]struct{}{changedPathSourceKey(file): {}}, result.CachedSourceKeys)
	assert.Empty(t, result.CachedFallbackProviders)
	assert.NotContains(t, provider.calls, "parse")
}

func TestSyncChangedPathPlanFullParseScopeUsesDurableAttemptCache(t *testing.T) {
	for _, tc := range []struct {
		name           string
		plan           func(parser.DiscoveredFile) ChangedPathPlan
		options        func(parser.DiscoveredFile) ChangedPathSyncOptions
		fallback       bool
		cached         bool
		planForceParse bool
	}{
		{
			name: "exact source",
			plan: func(file parser.DiscoveredFile) ChangedPathPlan {
				return ChangedPathPlan{Files: []parser.DiscoveredFile{file}}
			},
			options: func(file parser.DiscoveredFile) ChangedPathSyncOptions {
				return ChangedPathSyncOptions{ForceFullParse: ChangedPathPruneScope{
					Files: []parser.DiscoveredFile{file},
				}}
			},
		},
		{
			name: "fallback provider",
			plan: func(parser.DiscoveredFile) ChangedPathPlan {
				return ChangedPathPlan{FallbackProviders: []parser.AgentType{
					parser.AgentCowork,
				}}
			},
			options: func(parser.DiscoveredFile) ChangedPathSyncOptions {
				return ChangedPathSyncOptions{ForceFullParse: ChangedPathPruneScope{
					FallbackProviders: []parser.AgentType{parser.AgentCowork},
				}}
			},
			fallback: true,
		},
		{
			name: "exact source already attempted",
			plan: func(file parser.DiscoveredFile) ChangedPathPlan {
				return ChangedPathPlan{Files: []parser.DiscoveredFile{file}}
			},
			options: func(file parser.DiscoveredFile) ChangedPathSyncOptions {
				return ChangedPathSyncOptions{ForceFullParse: ChangedPathPruneScope{
					Files: []parser.DiscoveredFile{file},
				}}
			},
			cached:         true,
			planForceParse: true,
		},
		{
			name: "fallback source already attempted",
			plan: func(parser.DiscoveredFile) ChangedPathPlan {
				return ChangedPathPlan{FallbackProviders: []parser.AgentType{
					parser.AgentCowork,
				}}
			},
			options: func(parser.DiscoveredFile) ChangedPathSyncOptions {
				return ChangedPathSyncOptions{ForceFullParse: ChangedPathPruneScope{
					FallbackProviders: []parser.AgentType{parser.AgentCowork},
				}}
			},
			fallback: true,
			cached:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			path, fingerprint := writeProcessProviderSource(t, root, "armed.jsonl")
			source := processFixtureSource(path)
			provider := newProcessFixtureProvider(
				source, fingerprint, parser.ParseOutcome{ResultSetComplete: true},
			)
			provider.Caps.Sync.SkipCacheFreshWithoutStoredRow = true
			if tc.fallback {
				provider.discovered = []parser.SourceRef{source}
			}
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentCowork: {root},
				},
				Machine: "remote", Ephemeral: true,
				ProviderFactories: []parser.ProviderFactory{
					processFixtureFactory{provider: provider},
				},
				ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
					parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
				},
			})
			t.Cleanup(engine.Close)
			if tc.cached {
				engine.skipCache[providerAgentSkipCacheKey(path, parser.AgentCowork)] = fingerprint.MTimeNS
			}
			file := parser.DiscoveredFile{
				Path: path, Agent: parser.AgentCowork,
				ProviderSource: &source, ProviderProcess: true,
				ForceParse: tc.planForceParse,
			}

			result, err := engine.SyncChangedPathPlanWithOptionsContext(
				t.Context(), tc.plan(file), tc.options(file), nil,
			)
			require.NoError(t, err)
			assert.Equal(t, 1, result.FilesProcessed)
			if tc.cached {
				assert.Equal(t, 1, result.Stats.Skipped)
				assert.Empty(t, provider.parseRequests)
			} else {
				assert.Zero(t, result.Stats.Skipped)
				require.Len(t, provider.parseRequests, 1)
				assert.True(t, provider.parseRequests[0].ForceParse)
			}
		})
	}
}

func TestSyncChangedPathPlanFallbackDiscoveryStaysProviderBounded(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	var callsA, callsB int
	caps := changedPathPlanCapabilities()
	sourcePath := filepath.Join(rootA, "fallback.jsonl")
	factoryA := changedPathPlanFactory{agent: "fallback-a", caps: caps}
	factoryA.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{
			discoveries: &callsA,
			discovered: []parser.SourceRef{{
				Provider: factoryA.agent, Key: sourcePath,
				DisplayPath: sourcePath, FingerprintKey: sourcePath,
			}},
		}
	}
	factoryB := changedPathPlanFactory{agent: "fallback-b", caps: caps}
	factoryB.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
		return &changedPathPlanProvider{discoveries: &callsB}
	}
	engine := newChangedPathPlanEngine(t, map[parser.AgentType][]string{
		factoryA.agent: {rootA}, factoryB.agent: {rootB},
	}, factoryA, factoryB)
	t.Cleanup(engine.Close)
	plan := ChangedPathPlan{FallbackProviders: []parser.AgentType{factoryA.agent}}

	result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
	require.Error(t, err, "the fake has no fingerprint implementation")
	assert.Equal(t, 1, result.FilesDiscovered)
	assert.Equal(t, 1, result.FilesProcessed)
	assert.Equal(t, 1, result.Stats.Failed)
	assert.Equal(t, 1, callsA)
	assert.Zero(t, callsB)
}

func TestSyncChangedPathPlanRemoteForceReplacePreservesMissingOwnedMember(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	path, fingerprint := writeProcessProviderSource(t, root, "container.jsonl")
	source := processFixtureSource(path)
	provider := newProcessFixtureProvider(source, fingerprint, parser.ParseOutcome{
		SkipReason:        parser.SkipNonInteractive,
		ResultSetComplete: true,
		ForceReplace:      true,
	})
	storedPath := "remote:" + path
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "cowork:missing", Agent: string(parser.AgentCowork),
		Project: "fixture-project", Machine: "remote", FilePath: &storedPath,
	}))
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCowork: {root}},
		Machine:   "remote", Ephemeral: true,
		ProviderFactories: []parser.ProviderFactory{processFixtureFactory{provider: provider}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
		},
		PathRewriter: func(value string) string { return "remote:" + value },
	})
	t.Cleanup(engine.Close)
	file := parser.DiscoveredFile{
		Path: path, Agent: parser.AgentCowork,
		ProviderSource: &source, ProviderProcess: true,
	}

	result, err := engine.SyncChangedPathPlanContext(t.Context(), ChangedPathPlan{
		Files: []parser.DiscoveredFile{file},
	}, nil)
	require.NoError(t, err)
	assert.Zero(t, result.Stats.Failed)
	retained, err := database.GetSession(t.Context(), "cowork:missing")
	require.NoError(t, err)
	require.NotNil(t, retained)
	assert.Nil(t, retained.DeletedAt)
}

func TestRemoteChangedPathWorkBoundedByPlannedSources(t *testing.T) {
	run := func(t *testing.T, unrelatedRoots int) (
		providerCalls, unrelatedDiscoveries, globalLinkPasses int,
	) {
		t.Helper()
		database := dbtest.OpenTestDB(t)
		root := t.TempDir()
		path := filepath.Join(root, "planned.jsonl")
		fingerprint := parser.SourceFingerprint{Key: path, Size: 7, MTimeNS: 42}
		source := parser.SourceRef{
			Provider: parser.AgentCowork, Key: path,
			DisplayPath: path, FingerprintKey: path,
		}
		provider := newProcessFixtureProvider(source, fingerprint, parser.ParseOutcome{})
		provider.Caps.Sync.SkipCacheFreshWithoutStoredRow = true
		unrelatedAgent := parser.AgentType("unrelated-provider")
		unrelatedFactory := changedPathPlanFactory{
			agent: unrelatedAgent, caps: changedPathPlanCapabilities(),
		}
		unrelatedFactory.provider = func(parser.ProviderConfig) *changedPathPlanProvider {
			return &changedPathPlanProvider{discoveries: &unrelatedDiscoveries}
		}
		roots := make([]string, 0, unrelatedRoots)
		for i := range unrelatedRoots {
			roots = append(roots, filepath.Join(root, "unrelated", fmt.Sprintf("%04d", i)))
		}
		engine := NewEngine(t.Context(), database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentCowork: {root}, unrelatedAgent: roots,
			},
			Machine: "remote", Ephemeral: true,
			ProviderFactories: []parser.ProviderFactory{
				processFixtureFactory{provider: provider}, unrelatedFactory,
			},
			ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
				parser.AgentCowork: parser.ProviderMigrationProviderAuthoritative,
				unrelatedAgent:     parser.ProviderMigrationProviderAuthoritative,
			},
		})
		t.Cleanup(engine.Close)
		engine.skipCache[providerAgentSkipCacheKey(path, parser.AgentCowork)] = fingerprint.MTimeNS
		file := parser.DiscoveredFile{
			Path: path, Agent: parser.AgentCowork,
			ProviderSource: &source, ProviderProcess: true,
		}
		metrics := &reconciliationRuntimeMetrics{}
		ctx := context.WithValue(
			t.Context(), reconciliationMetricsContextKey{}, metrics,
		)
		result, err := engine.SyncChangedPathPlanContext(ctx, ChangedPathPlan{
			Files: []parser.DiscoveredFile{file},
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, result.FilesProcessed)
		return len(provider.calls), unrelatedDiscoveries,
			metrics.snapshot(ReconciliationMetrics{}).GlobalLinkPasses
	}

	var baselineCalls int
	for _, cardinality := range []int{1, 1_000, 10_000} {
		calls, unrelated, globalLinks := run(t, cardinality)
		if cardinality == 1 {
			baselineCalls = calls
		}
		assert.Equal(t, baselineCalls, calls)
		assert.Zero(t, unrelated)
		assert.Zero(t, globalLinks,
			"bounded changed-path work must not invoke archive-wide linking")
	}
}
