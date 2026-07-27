package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestReconciliationBaselineFailureCannotPromoteZeroResultCache(
	t *testing.T,
) {
	testReconciliationBaselineFailureCannotPromoteNoWriteCache(t, false)
}

func TestReconciliationBaselineFailureCannotPromoteUnchangedResultCache(
	t *testing.T,
) {
	testReconciliationBaselineFailureCannotPromoteNoWriteCache(t, true)
}

func TestNonStreamedBaselineFailureCannotPromoteZeroResultCache(t *testing.T) {
	const agent parser.AgentType = "no-write-baseline"
	database := openTestDB(t)
	path := filepath.Join(t.TempDir(), "container.db")
	engine := NewEngine(database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	results := make(chan syncJob, 1)
	results <- syncJob{
		agent: agent,
		path:  path,
		processResult: processResult{
			cacheSkip: true,
			cacheKey:  path + "?complete",
			mtime:     1234,
		},
	}
	close(results)
	require.NoError(t, database.CloseWriter())

	stats := engine.collectAndBatch(
		t.Context(), results, 1, 1, nil, syncWriteDefault,
	)

	require.NoError(t, database.ReopenWriter())
	assert.Positive(t, stats.Failed)
	assert.Empty(t, engine.SnapshotSkipCache(),
		"a failed ordinary baseline must reject zero-result cache freshness")
}

func testReconciliationBaselineFailureCannotPromoteNoWriteCache(
	t *testing.T,
	unchanged bool,
) {
	t.Helper()
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "container.db")
	require.NoError(t, os.WriteFile(container, []byte("container"), 0o600))
	source := parser.SourceRef{
		Provider:       semanticTestAgent,
		Key:            container,
		DisplayPath:    container,
		FingerprintKey: container,
	}
	fingerprint := parser.SourceFingerprint{
		Key: container, Size: 9, MTimeNS: 1234, Hash: "container-hash",
	}
	memberPath := container + "#unchanged"
	var results []parser.ParseResultOutcome
	unchangedPolicy := parser.UnchangedResultNone
	if unchanged {
		size := fingerprint.Size
		mtime := fingerprint.MTimeNS
		hash := fingerprint.Hash
		require.NoError(t, database.UpsertSession(db.Session{
			ID: "semantic:unchanged", Agent: string(semanticTestAgent),
			Machine: "devbox", Project: "semantic-project",
			FilePath: &memberPath, FileSize: &size, FileMtime: &mtime,
			FileHash: &hash,
		}))
		require.NoError(t, database.SetSessionDataVersion(
			"semantic:unchanged", db.CurrentDataVersion(),
		))
		unchangedResult := semanticTestResult(
			"semantic:unchanged", memberPath, fingerprint,
		)
		unchangedResult.Result.Session.File.Hash = fingerprint.Hash
		results = []parser.ParseResultOutcome{unchangedResult}
		unchangedPolicy = parser.UnchangedResultMTimeAndHash
	}
	provider := &semanticTestProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{
				Type: semanticTestAgent, IDPrefix: "semantic:", FileBased: true,
			},
			Caps: parser.Capabilities{
				Source: parser.SourceCapabilities{
					DiscoverSources:    parser.CapabilitySupported,
					StreamingDiscovery: parser.CapabilitySupported,
					MultiSessionSource: parser.CapabilitySupported,
				},
				Sync: parser.ProviderSyncSemantics{
					FingerprintHashInCacheKey: true,
					UnchangedResults:          unchangedPolicy,
				},
			},
		},
		sources: []parser.SourceRef{source},
		reconciled: map[string]parser.SourceRef{
			container: source,
		},
		semantics: map[string]parser.SourceSyncSemantics{
			container: {
				BackingContainerPath:           container,
				CacheAfterWrite:                true,
				CacheKeyIncludesDataVersion:    true,
				SkipCacheFreshWithoutStoredRow: true,
			},
		},
		fingerprints: map[string]parser.SourceFingerprint{
			container: fingerprint,
		},
		outcomes: map[string]parser.ParseOutcome{
			container: {
				Results:           results,
				ResultSetComplete: true,
			},
		},
	}
	engine := newSemanticTestEngine(t, database, root, provider)
	require.NoError(t, database.CloseWriter())

	err := engine.ReconcileWatchRoots(
		t.Context(), []string{root}, true,
	)

	require.Error(t, err)
	require.NoError(t, database.ReopenWriter())
	assert.Equal(t, 1, provider.parseCalls,
		"the regression must exercise a complete parsed outcome with no write")
	assert.Empty(t, engine.SnapshotSkipCache(),
		"no-write cache freshness must wait for the ownership baseline")
}

func TestReconciliationBaselineFailureCannotPromoteContainerCache(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "container.db")
	require.NoError(t, os.WriteFile(container, []byte("{}"), 0o600))
	source := parser.SourceRef{
		Provider:       semanticTestAgent,
		Key:            container,
		DisplayPath:    container,
		FingerprintKey: container,
	}
	fingerprint := parser.SourceFingerprint{
		Key: container, Size: 2, MTimeNS: 1234, Hash: "container-hash",
	}
	provider := &semanticTestProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{
				Type: semanticTestAgent, IDPrefix: "semantic:", FileBased: true,
			},
			Caps: parser.Capabilities{
				Source: parser.SourceCapabilities{
					DiscoverSources:    parser.CapabilitySupported,
					StreamingDiscovery: parser.CapabilitySupported,
					MultiSessionSource: parser.CapabilitySupported,
				},
				Sync: parser.ProviderSyncSemantics{
					FingerprintHashInCacheKey: true,
				},
			},
		},
		sources: []parser.SourceRef{source},
		reconciled: map[string]parser.SourceRef{
			container: source,
		},
		semantics: map[string]parser.SourceSyncSemantics{
			container: {
				BackingContainerPath:           container,
				CacheAfterWrite:                true,
				CacheKeyIncludesDataVersion:    true,
				SkipCacheFreshWithoutStoredRow: true,
			},
		},
		fingerprints: map[string]parser.SourceFingerprint{
			container: fingerprint,
		},
		outcomes: map[string]parser.ParseOutcome{
			container: {
				Results: []parser.ParseResultOutcome{semanticTestResult(
					"semantic:session", container+"#session", fingerprint,
				)},
				ResultSetComplete: true,
				ForceReplace:      true,
			},
		},
	}
	engine := newSemanticTestEngine(t, database, root, provider)
	engine.writeBatchOverride = func(
		batch []pendingWrite, _ syncWriteMode, _ bool,
	) (int, int, int, int) {
		return len(batch), 0, 0, 0
	}
	require.NoError(t, database.CloseWriter())

	err := engine.ReconcileWatchRoots(t.Context(), []string{root}, true)

	require.Error(t, err)
	require.NoError(t, database.ReopenWriter())
	assert.Empty(t, engine.SnapshotSkipCache(),
		"rowless cache freshness must wait for durable ownership baselines")
}

func TestCompleteResultOwnershipReadFailureAbortsWithoutCaching(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "shared.db")
	require.NoError(t, os.WriteFile(container, []byte("container"), 0o600))
	memberPath := container + "#stored"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "semantic:stored", Agent: string(semanticTestAgent),
		Machine: "devbox", FilePath: &memberPath,
	}))
	source := parser.SourceRef{
		Provider: semanticTestAgent,
		Key:      container, DisplayPath: container, FingerprintKey: container,
	}
	fingerprint := parser.SourceFingerprint{
		Key: container, Size: 9, MTimeNS: 1234, Hash: "ownership-hash",
	}
	provider := &semanticTestProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{
				Type: semanticTestAgent, IDPrefix: "semantic:", FileBased: true,
			},
			Caps: parser.Capabilities{
				Source: parser.SourceCapabilities{
					MultiSessionSource: parser.CapabilitySupported,
				},
				Sync: parser.ProviderSyncSemantics{
					FingerprintHashInCacheKey: true,
				},
			},
		},
		semantics: map[string]parser.SourceSyncSemantics{
			container: {
				BackingContainerPath:           container,
				CompleteResultOwnsMembers:      true,
				CacheAfterWrite:                true,
				CacheKeyIncludesDataVersion:    true,
				SkipCacheFreshWithoutStoredRow: true,
			},
		},
		scopes: []parser.StoredSourceHintScope{{
			Path: container, IncludeVirtualMembers: true,
		}},
		fingerprints: map[string]parser.SourceFingerprint{
			container: fingerprint,
		},
		outcomes: map[string]parser.ParseOutcome{
			container: {
				ResultSetComplete: true,
				ForceReplace:      true,
			},
		},
	}
	provider.beforeParse = func() {
		require.NoError(t, database.CloseConnections())
	}
	engine := newSemanticTestEngine(t, database, root, provider)
	file := parser.DiscoveredFile{
		Path: container, Agent: semanticTestAgent,
		ProviderSource: &source, ProviderProcess: true,
	}

	result := engine.processFile(t.Context(), file)

	require.NoError(t, database.Reopen())
	require.Error(t, result.err)
	assert.Empty(t, engine.SnapshotSkipCache())
}
