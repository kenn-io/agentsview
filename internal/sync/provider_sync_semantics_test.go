// ABOUTME: Tests provider-declared source sync semantics in the sync engine.
// ABOUTME: Covers persistent containers and dependent-source expansion.
package sync

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const semanticTestAgent parser.AgentType = "semantic-sync-test"

type semanticTestFactory struct {
	provider *semanticTestProvider
}

func (f semanticTestFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f semanticTestFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

func (f semanticTestFactory) NewProvider(parser.ProviderConfig) parser.Provider {
	return f.provider
}

type semanticTestProvider struct {
	parser.ProviderBase
	sources       []parser.SourceRef
	changed       []parser.SourceRef
	reconciled    map[string]parser.SourceRef
	semantics     map[string]parser.SourceSyncSemantics
	dependentIDs  map[string]string
	fingerprints  map[string]parser.SourceFingerprint
	outcomes      map[string]parser.ParseOutcome
	restore       func(context.Context, parser.SourceRef) (bool, error)
	beforeParse   func()
	scopes        []parser.StoredSourceHintScope
	parseRequests []parser.ParseRequest
	parseCalls    int
	restoreCalls  int
}

func (p *semanticTestProvider) Parse(
	_ context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	if p.beforeParse != nil {
		p.beforeParse()
	}
	p.parseCalls++
	p.parseRequests = append(p.parseRequests, req)
	return p.outcomes[req.Source.Key], nil
}

func (p *semanticTestProvider) DiscoverEach(
	ctx context.Context,
	yield func(parser.SourceRef) error,
) error {
	for _, source := range p.sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(source); err != nil {
			return err
		}
	}
	return nil
}

func (p *semanticTestProvider) Discover(
	context.Context,
) ([]parser.SourceRef, error) {
	return append([]parser.SourceRef(nil), p.sources...), nil
}

func (p *semanticTestProvider) SourcesForChangedPath(
	context.Context, parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	return append([]parser.SourceRef(nil), p.changed...), nil
}

func (p *semanticTestProvider) Fingerprint(
	_ context.Context, source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return p.fingerprints[source.Key], nil
}

func (p *semanticTestProvider) SourceSyncSemantics(
	source parser.SourceRef,
) parser.SourceSyncSemantics {
	return p.semantics[source.Key]
}

func (p *semanticTestProvider) DependentSourceRootSessionID(
	source parser.SourceRef,
) (string, bool) {
	id, ok := p.dependentIDs[source.Key]
	return id, ok
}

func (p *semanticTestProvider) SourceForReconciliation(
	_ context.Context, path, _ string,
) (parser.SourceRef, bool, error) {
	source, ok := p.reconciled[path]
	return source, ok, nil
}

func (p *semanticTestProvider) RestoreCachedSourceState(
	ctx context.Context, source parser.SourceRef,
) (bool, error) {
	p.restoreCalls++
	if p.restore == nil {
		return false, nil
	}
	return p.restore(ctx, source)
}

func (p *semanticTestProvider) StoredSourceHintScopes(
	parser.ChangedPathRequest,
) []parser.StoredSourceHintScope {
	return append([]parser.StoredSourceHintScope(nil), p.scopes...)
}

func semanticTestResult(
	id string,
	path string,
	fingerprint parser.SourceFingerprint,
) parser.ParseResultOutcome {
	return parser.ParseResultOutcome{
		Result: processFixtureResult(
			id, semanticTestAgent, "semantic-project", path, fingerprint,
		),
		DataVersion: parser.DataVersionCurrent,
	}
}

func newSemanticTestEngine(
	t *testing.T,
	database *db.DB,
	root string,
	provider *semanticTestProvider,
) *Engine {
	t.Helper()
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			semanticTestAgent: {root},
		},
		Machine:           "devbox",
		ProviderFactories: []parser.ProviderFactory{semanticTestFactory{provider: provider}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			semanticTestAgent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	return engine
}

func collectSemanticTestResult(
	engine *Engine,
	file parser.DiscoveredFile,
	result processResult,
) SyncStats {
	results := make(chan syncJob, 1)
	results <- syncJob{
		path: file.Path, agent: file.Agent, processResult: result,
	}
	close(results)
	return engine.collectAndBatch(
		context.Background(), results, 1, 1, nil, syncWriteDefault,
	)
}

func TestSyncSemanticsWholeContainerCachePromotesAfterSuccessfulWrite(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "shared.db")
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
	memberOne := container + "#one"
	memberTwo := container + "#two"
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
				Results: []parser.ParseResultOutcome{
					semanticTestResult("semantic:one", memberOne, fingerprint),
					semanticTestResult("semantic:two", memberTwo, fingerprint),
				},
				ResultSetComplete: true,
			},
		},
	}
	engine := newSemanticTestEngine(t, database, root, provider)
	file := parser.DiscoveredFile{
		Path: container, Agent: semanticTestAgent,
		ProviderSource: &source, ProviderProcess: true,
	}

	result := engine.processFile(t.Context(), file)

	require.NoError(t, result.err)
	require.Len(t, result.results, 2)
	assert.Empty(t, engine.SnapshotSkipCache(),
		"container cache must not be promoted before member writes")

	stats := collectSemanticTestResult(engine, file, result)

	assert.Zero(t, stats.Failed)
	assert.Equal(t, 2, stats.Synced)
	for _, id := range []string{"semantic:one", "semantic:two"} {
		stored, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.NotNil(t, stored)
	}
	wantKey := container + "?source_hash=container-hash&data_version=" +
		strconv.Itoa(db.CurrentDataVersion())
	assert.Equal(t, map[string]int64{wantKey: fingerprint.MTimeNS},
		engine.SnapshotSkipCache())
}

func TestSyncSemanticsCachedStateRestorationReparsesChangedSource(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "shared.db")
	source := parser.SourceRef{
		Provider:       semanticTestAgent,
		Key:            container,
		DisplayPath:    container,
		FingerprintKey: container,
	}
	initial := parser.SourceFingerprint{
		Key: container, Size: 10, MTimeNS: 4321, Hash: "before-restore",
	}
	restored := initial
	restored.Hash = "after-restore"
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
					FingerprintHashInCacheKey:           true,
					FingerprintHashRequiredForFreshness: true,
				},
			},
		},
		semantics: map[string]parser.SourceSyncSemantics{
			container: {
				CacheAfterWrite:                true,
				CacheKeyIncludesDataVersion:    true,
				SkipCacheFreshWithoutStoredRow: true,
			},
		},
		fingerprints: map[string]parser.SourceFingerprint{
			container: initial,
		},
		outcomes: map[string]parser.ParseOutcome{
			container: {
				Results: []parser.ParseResultOutcome{
					semanticTestResult(
						"semantic:restored", container+"#restored", restored,
					),
				},
				ResultSetComplete: true,
			},
		},
	}
	provider.restore = func(
		context.Context, parser.SourceRef,
	) (bool, error) {
		provider.fingerprints[container] = restored
		return true, nil
	}
	engine := newSemanticTestEngine(t, database, root, provider)
	initialKey := providerProcessCacheKey(
		parser.DiscoveredFile{Path: container, Agent: semanticTestAgent},
		source,
		initial,
		provider.Capabilities().Sync,
		provider.SourceSyncSemantics(source),
	)
	engine.cacheSkip(initialKey, initial.MTimeNS)
	file := parser.DiscoveredFile{
		Path: container, Agent: semanticTestAgent,
		ProviderSource: &source, ProviderProcess: true,
	}

	result := engine.processFile(t.Context(), file)

	require.NoError(t, result.err)
	assert.False(t, result.skip)
	assert.Equal(t, 1, provider.restoreCalls)
	assert.Equal(t, 1, provider.parseCalls)
	require.Len(t, provider.parseRequests, 1)
	assert.Equal(t, restored, provider.parseRequests[0].Fingerprint)
	require.Len(t, result.results, 1)
	assert.Empty(t, engine.SnapshotSkipCache(),
		"stale pre-restoration cache entry must be cleared")

	stats := collectSemanticTestResult(engine, file, result)

	assert.Zero(t, stats.Failed)
	restoredKey := providerProcessCacheKey(
		file,
		source,
		restored,
		provider.Capabilities().Sync,
		provider.SourceSyncSemantics(source),
	)
	assert.Equal(t, map[string]int64{restoredKey: restored.MTimeNS},
		engine.SnapshotSkipCache())
}

func TestSyncSemanticsDeclaredRowlessCacheFreshnessSkipsParse(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "rowless.jsonl")
	source := parser.SourceRef{
		Provider:       semanticTestAgent,
		Key:            path,
		DisplayPath:    path,
		FingerprintKey: path,
	}
	fingerprint := parser.SourceFingerprint{
		Key: path, Size: 10, MTimeNS: 2468, Hash: "rowless-hash",
	}
	provider := &semanticTestProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{
				Type: semanticTestAgent, IDPrefix: "semantic:", FileBased: true,
			},
			Caps: parser.Capabilities{
				Sync: parser.ProviderSyncSemantics{
					FingerprintHashInCacheKey:           true,
					FingerprintHashRequiredForFreshness: true,
					SkipCacheFreshWithoutStoredRow:      true,
				},
			},
		},
		fingerprints: map[string]parser.SourceFingerprint{
			path: fingerprint,
		},
	}
	engine := newSemanticTestEngine(t, database, root, provider)
	file := parser.DiscoveredFile{
		Path: path, Agent: semanticTestAgent,
		ProviderSource: &source, ProviderProcess: true,
	}
	cacheKey := providerProcessCacheKey(
		file,
		source,
		fingerprint,
		provider.Capabilities().Sync,
		provider.SourceSyncSemantics(source),
	)
	engine.cacheSkip(cacheKey, fingerprint.MTimeNS)

	result := engine.processFile(t.Context(), file)

	require.NoError(t, result.err)
	assert.True(t, result.skip)
	assert.Zero(t, provider.parseCalls)
}

func TestSyncSemanticsCompleteResultOwnershipTombstonesAndRevivesMissingMember(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "shared.db")
	require.NoError(t, os.WriteFile(container, []byte("container"), 0o600))
	source := parser.SourceRef{
		Provider:       semanticTestAgent,
		Key:            container,
		DisplayPath:    container,
		FingerprintKey: container,
	}
	fingerprint := parser.SourceFingerprint{
		Key: container, Size: 9, MTimeNS: 2222, Hash: "ownership-hash",
	}
	keptPath := container + "#kept"
	missingPath := container + "#missing"
	for _, seed := range []db.Session{
		{
			ID: "semantic:kept", Agent: string(semanticTestAgent),
			Machine: "devbox", FilePath: &keptPath,
		},
		{
			ID: "semantic:missing", Agent: string(semanticTestAgent),
			Machine: "devbox", FilePath: &missingPath,
		},
	} {
		require.NoError(t, database.UpsertSession(seed))
	}
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "devbox", []db.SessionSourcePath{
			{Agent: string(semanticTestAgent), FilePath: keptPath},
			{Agent: string(semanticTestAgent), FilePath: missingPath},
		},
	))
	provider := &semanticTestProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{
				Type: semanticTestAgent, IDPrefix: "semantic:", FileBased: true,
			},
			Caps: parser.Capabilities{
				Source: parser.SourceCapabilities{
					MultiSessionSource: parser.CapabilitySupported,
				},
			},
		},
		semantics: map[string]parser.SourceSyncSemantics{
			container: {
				BackingContainerPath:      container,
				CompleteResultOwnsMembers: true,
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
				Results: []parser.ParseResultOutcome{
					semanticTestResult("semantic:kept", keptPath, fingerprint),
				},
				ResultSetComplete: true,
				ForceReplace:      true,
			},
		},
	}
	engine := newSemanticTestEngine(t, database, root, provider)
	file := parser.DiscoveredFile{
		Path: container, Agent: semanticTestAgent,
		ProviderSource: &source, ProviderProcess: true,
	}

	first := engine.processFile(t.Context(), file)
	require.NoError(t, first.err)
	firstStats := collectSemanticTestResult(engine, file, first)
	require.Zero(t, firstStats.Failed)

	active, err := database.GetSession(t.Context(), "semantic:missing")
	require.NoError(t, err)
	assert.Nil(t, active)
	archived, err := database.GetSessionFull(t.Context(), "semantic:missing")
	require.NoError(t, err)
	require.NotNil(t, archived)
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)

	provider.outcomes[container] = parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{
			semanticTestResult("semantic:kept", keptPath, fingerprint),
			semanticTestResult("semantic:missing", missingPath, fingerprint),
		},
		ResultSetComplete: true,
		ForceReplace:      true,
	}
	second := engine.processFile(t.Context(), file)
	require.NoError(t, second.err)
	secondStats := collectSemanticTestResult(engine, file, second)
	require.Zero(t, secondStats.Failed)

	revived, err := database.GetSession(t.Context(), "semantic:missing")
	require.NoError(t, err)
	assert.NotNil(t, revived)
}

func TestSyncSemanticsUnchangedResultPolicies(t *testing.T) {
	database := openTestDB(t)
	path := filepath.Join(t.TempDir(), "shared.db#member")
	size := int64(10)
	mtime := int64(5678)
	storedHash := "stored-hash"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "semantic:member", Agent: string(semanticTestAgent),
		Machine: "devbox", FilePath: &path, FileSize: &size,
		FileMtime: &mtime, FileHash: &storedHash,
	}))
	require.NoError(t, database.SetSessionDataVersion(
		"semantic:member", db.CurrentDataVersion(),
	))
	result := processFixtureResult(
		"semantic:member", semanticTestAgent, "semantic-project", path,
		parser.SourceFingerprint{
			Key: path, Size: size, MTimeNS: mtime, Hash: "changed-hash",
		},
	)
	result.Session.File.Hash = "changed-hash"
	engine := &Engine{db: database}
	file := parser.DiscoveredFile{Agent: semanticTestAgent, Path: path}

	mtimeOnly := engine.dropUnchangedSharedSQLiteResults(
		file, []parser.ParseResult{result}, parser.UnchangedResultMTime,
	)
	mtimeAndHash := engine.dropUnchangedSharedSQLiteResults(
		file, []parser.ParseResult{result},
		parser.UnchangedResultMTimeAndHash,
	)

	assert.Empty(t, mtimeOnly)
	require.Len(t, mtimeAndHash, 1)
	assert.Equal(t, "semantic:member", mtimeAndHash[0].Session.ID)
}

func TestSyncSemanticsPersistentContainerProtectsMissingPath(t *testing.T) {
	container := filepath.Join(t.TempDir(), "shared.db")
	source := parser.SourceRef{
		Provider:       semanticTestAgent,
		Key:            container,
		DisplayPath:    container,
		FingerprintKey: container,
	}
	provider := &semanticTestProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{Type: semanticTestAgent, FileBased: true},
		},
		semantics: map[string]parser.SourceSyncSemantics{
			container: {
				BackingContainerPath: container,
				PersistentContainer:  true,
			},
		},
	}
	engine := &Engine{
		providerFactories: map[parser.AgentType]parser.ProviderFactory{
			semanticTestAgent: semanticTestFactory{provider: provider},
		},
		agentDirs: map[parser.AgentType][]string{
			semanticTestAgent: {filepath.Dir(container)},
		},
	}

	got := engine.omitMissingPersistentContainerPaths(
		[]string{container, container + ".other"},
		[]parser.DiscoveredFile{{
			Path: container, Agent: semanticTestAgent, ProviderSource: &source,
		}},
	)

	assert.Equal(t, []string{container + ".other"}, got)
}

func TestProviderDependentSourceExpansionPreservesEngineIDPrefixing(
	t *testing.T,
) {
	database := openTestDB(t)
	rootPath := filepath.Join(t.TempDir(), "root.source")
	childPath := filepath.Join(filepath.Dir(rootPath), "child.source")
	rootSource := parser.SourceRef{
		Provider:       semanticTestAgent,
		Key:            rootPath,
		DisplayPath:    rootPath,
		FingerprintKey: rootPath,
	}
	childSource := parser.SourceRef{
		Provider:       semanticTestAgent,
		Key:            childPath,
		DisplayPath:    childPath,
		FingerprintKey: childPath,
	}
	provider := &semanticTestProvider{
		ProviderBase: parser.ProviderBase{
			Def: parser.AgentDef{Type: semanticTestAgent, FileBased: true},
		},
		reconciled: map[string]parser.SourceRef{
			childPath: childSource,
		},
		dependentIDs: map[string]string{
			rootPath: "provider:root",
		},
	}
	parentID := "remote~provider:root"
	require.NoError(t, database.UpsertSession(db.Session{
		ID:       parentID,
		Agent:    string(semanticTestAgent),
		Machine:  "local",
		FilePath: &rootPath,
	}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID:              "remote~provider:child",
		Agent:           string(semanticTestAgent),
		Machine:         "local",
		ParentSessionID: &parentID,
		FilePath:        &childPath,
	}))
	engine := &Engine{
		db:       database,
		machine:  "local",
		idPrefix: "remote~",
	}

	expanded, err := engine.expandProviderDependentSources(
		t.Context(), provider, []parser.SourceRef{rootSource},
	)

	require.NoError(t, err)
	assert.ElementsMatch(t, []parser.SourceRef{rootSource, childSource}, expanded)
}
