// ABOUTME: Tests the engine's direct omnigent source handling: cache-after-
// ABOUTME: write, ownership tombstones, dependent sources, baseline gating.
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

type semanticTestScopedProvider struct {
	*semanticTestProvider
	scopes parser.ProviderBase
}

func (p semanticTestScopedProvider) ResolveReconciliationScopes(
	ctx context.Context, req parser.ReconciliationScopeRequest,
) (parser.ReconciliationScopePlan, error) {
	return p.scopes.ResolveReconciliationScopes(ctx, req)
}

func (f semanticTestFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	return semanticTestScopedProvider{
		semanticTestProvider: f.provider,
		scopes:               perCallScopeProviderBase(f.provider.ProviderBase, cfg),
	}
}

type semanticTestProvider struct {
	parser.ProviderBase
	sources       []parser.SourceRef
	reconciled    map[string]parser.SourceRef
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

func (p *semanticTestProvider) Fingerprint(
	_ context.Context, source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return p.fingerprints[source.Key], nil
}

func (p *semanticTestProvider) SourceForReconciliation(
	_ context.Context, path, _ string,
) (parser.SourceRef, bool, error) {
	source, ok := p.reconciled[path]
	return source, ok, nil
}

// RestoreCachedSourceState is reached through the structural fallback in
// parser.RestoreOmnigentCachedSourceState, mirroring how test decorators wrap
// the real provider.
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
			id, parser.AgentOmnigent, "semantic-project", path, fingerprint,
		),
		DataVersion: parser.DataVersionCurrent,
	}
}

// newContainerSemanticProvider builds the fake omnigent container provider the
// engine-direct tests share: one whole-container source with a declared
// fingerprint and parse outcome. The engine special-cases AgentOmnigent
// directly, so the fake only has to claim the agent type and container-shaped
// source paths.
func newContainerSemanticProvider(
	container string,
	fingerprint parser.SourceFingerprint,
	outcome parser.ParseOutcome,
) (*semanticTestProvider, parser.SourceRef) {
	source := parser.SourceRef{
		Provider:       parser.AgentOmnigent,
		Key:            container,
		DisplayPath:    container,
		FingerprintKey: container,
	}
	provider := &semanticTestProvider{
		Def: parser.AgentDef{
			Type:      parser.AgentOmnigent,
			IDPrefix:  "omnigent:",
			FileBased: true,
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
		sources:      []parser.SourceRef{source},
		reconciled:   map[string]parser.SourceRef{container: source},
		fingerprints: map[string]parser.SourceFingerprint{container: fingerprint},
		outcomes:     map[string]parser.ParseOutcome{container: outcome},
		scopes: []parser.StoredSourceHintScope{{
			Path: container, IncludeVirtualMembers: true,
		}},
	}
	return provider, source
}

func newSemanticTestEngine(
	t *testing.T,
	database *db.DB,
	root string,
	provider *semanticTestProvider,
) *Engine {
	t.Helper()
	agent := provider.Definition().Type
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			agent: {root},
		},
		Machine:           "devbox",
		ProviderFactories: []parser.ProviderFactory{semanticTestFactory{provider: provider}},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
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

func TestOmnigentWholeContainerCachePromotesAfterSuccessfulWrite(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "chat.db")
	require.NoError(t, os.WriteFile(container, []byte("container"), 0o600))
	fingerprint := parser.SourceFingerprint{
		Key: container, Size: 9, MTimeNS: 1234, Hash: "container-hash",
	}
	memberOne := container + "#one"
	memberTwo := container + "#two"
	provider, source := newContainerSemanticProvider(
		container, fingerprint, parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{
				semanticTestResult("omnigent:one", memberOne, fingerprint),
				semanticTestResult("omnigent:two", memberTwo, fingerprint),
			},
			ResultSetComplete: true,
		},
	)
	engine := newSemanticTestEngine(t, database, root, provider)
	file := parser.DiscoveredFile{
		Path: container, Agent: parser.AgentOmnigent,
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
	for _, id := range []string{"omnigent:one", "omnigent:two"} {
		stored, err := database.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.NotNil(t, stored)
	}
	wantKey := container + "?agent=omnigent?source_hash=container-hash&data_version=" +
		strconv.Itoa(db.CurrentDataVersion())
	assert.Equal(t, map[string]int64{wantKey: fingerprint.MTimeNS},
		engine.SnapshotSkipCache())
}

func TestOmnigentCachedStateRestorationReparsesChangedSource(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "chat.db")
	initial := parser.SourceFingerprint{
		Key: container, Size: 10, MTimeNS: 4321, Hash: "before-restore",
	}
	restored := initial
	restored.Hash = "after-restore"
	provider, source := newContainerSemanticProvider(
		container, initial, parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{
				semanticTestResult(
					"omnigent:restored", container+"#restored", restored,
				),
			},
			ResultSetComplete: true,
		},
	)
	provider.Caps.Sync.FingerprintHashRequiredForFreshness = true
	provider.restore = func(
		context.Context, parser.SourceRef,
	) (bool, error) {
		provider.fingerprints[container] = restored
		return true, nil
	}
	engine := newSemanticTestEngine(t, database, root, provider)
	initialKey := providerProcessCacheKey(
		parser.DiscoveredFile{Path: container, Agent: parser.AgentOmnigent},
		source,
		initial,
		provider.Capabilities().Sync,
	)
	engine.cacheSkip(initialKey, initial.MTimeNS)
	file := parser.DiscoveredFile{
		Path: container, Agent: parser.AgentOmnigent,
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
	)
	assert.Equal(t, map[string]int64{restoredKey: restored.MTimeNS},
		engine.SnapshotSkipCache())
}

func TestOmnigentContainerSkipCacheEntryFreshWithoutStoredRow(t *testing.T) {
	database := openTestDB(t)
	container := filepath.Join(t.TempDir(), "chat.db")
	memberPath := container + "#member"
	engine := &Engine{db: database}
	providerSemantics := parser.ProviderSyncSemantics{
		FingerprintHashInCacheKey:           true,
		FingerprintHashRequiredForFreshness: true,
	}

	containerFresh, containerHashVerified := engine.providerSkipCacheEntryFreshInDB(
		parser.DiscoveredFile{Path: container, Agent: parser.AgentOmnigent},
		parser.SourceRef{
			Provider: parser.AgentOmnigent, Key: container,
			DisplayPath: container, FingerprintKey: container,
		},
		parser.SourceFingerprint{Key: container, Hash: "container-hash"},
		providerSemantics,
	)
	memberFresh, _ := engine.providerSkipCacheEntryFreshInDB(
		parser.DiscoveredFile{Path: memberPath, Agent: parser.AgentOmnigent},
		parser.SourceRef{
			Provider: parser.AgentOmnigent, Key: memberPath,
			DisplayPath: memberPath, FingerprintKey: memberPath,
		},
		parser.SourceFingerprint{Key: memberPath, Hash: "member-hash"},
		providerSemantics,
	)

	assert.True(t, containerFresh,
		"a whole-container entry needs no stored physical-path row")
	assert.False(t, containerHashVerified,
		"container identity freshness never compares a stored row hash, "+
			"so it must not authorize a stat-digest stamp")
	assert.False(t, memberFresh,
		"a virtual member entry must still validate against its stored row")
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
	)
	engine.cacheSkip(cacheKey, fingerprint.MTimeNS)

	result := engine.processFile(t.Context(), file)

	require.NoError(t, result.err)
	assert.True(t, result.skip)
	assert.Zero(t, provider.parseCalls)
}

func TestOmnigentCompleteResultOwnershipTombstonesAndRevivesMissingMember(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "chat.db")
	require.NoError(t, os.WriteFile(container, []byte("container"), 0o600))
	fingerprint := parser.SourceFingerprint{
		Key: container, Size: 9, MTimeNS: 2222, Hash: "ownership-hash",
	}
	keptPath := container + "#kept"
	missingPath := container + "#missing"
	for _, seed := range []db.Session{
		{
			ID: "omnigent:kept", Agent: string(parser.AgentOmnigent),
			Machine: "devbox", FilePath: &keptPath,
		},
		{
			ID: "omnigent:missing", Agent: string(parser.AgentOmnigent),
			Machine: "", FilePath: &missingPath,
		},
	} {
		require.NoError(t, database.UpsertSession(seed))
	}
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "devbox", []db.SessionSourcePath{{
			Agent: string(parser.AgentOmnigent), FilePath: keptPath,
		}},
	))
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "", []db.SessionSourcePath{{
			Agent: string(parser.AgentOmnigent), FilePath: missingPath,
		}},
	))
	provider, source := newContainerSemanticProvider(
		container, fingerprint, parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{
				semanticTestResult("omnigent:kept", keptPath, fingerprint),
			},
			ResultSetComplete: true,
			ForceReplace:      true,
		},
	)
	engine := newSemanticTestEngine(t, database, root, provider)
	file := parser.DiscoveredFile{
		Path: container, Agent: parser.AgentOmnigent,
		ProviderSource: &source, ProviderProcess: true,
	}

	first := engine.processFile(t.Context(), file)
	require.NoError(t, first.err)
	firstStats := collectSemanticTestResult(engine, file, first)
	require.Zero(t, firstStats.Failed)

	active, err := database.GetSession(t.Context(), "omnigent:missing")
	require.NoError(t, err)
	assert.NotNil(t, active)
	archived, err := database.GetSessionFull(t.Context(), "omnigent:missing")
	require.NoError(t, err)
	assert.Empty(t, archived.Machine)
	assertSourceMissingState(t, archived)

	// The revived member re-appears through a real container change: the
	// database fingerprint moves, so the promoted container cache entry no
	// longer matches.
	changed := fingerprint
	changed.MTimeNS = 3333
	changed.Hash = "ownership-hash-revived"
	provider.fingerprints[container] = changed
	provider.outcomes[container] = parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{
			semanticTestResult("omnigent:kept", keptPath, changed),
			semanticTestResult("omnigent:missing", missingPath, changed),
		},
		ResultSetComplete: true,
		ForceReplace:      true,
	}
	second := engine.processFile(t.Context(), file)
	require.NoError(t, second.err)
	secondStats := collectSemanticTestResult(engine, file, second)
	require.Zero(t, secondStats.Failed)

	revived, err := database.GetSession(t.Context(), "omnigent:missing")
	require.NoError(t, err)
	require.NotNil(t, revived)
	assert.Empty(t, revived.Machine)
}

func TestCompleteResultOwnershipReadFailureAbortsWithoutCaching(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "chat.db")
	require.NoError(t, os.WriteFile(container, []byte("container"), 0o600))
	memberPath := container + "#stored"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "omnigent:stored", Agent: string(parser.AgentOmnigent),
		Machine: "devbox", FilePath: &memberPath,
	}))
	fingerprint := parser.SourceFingerprint{
		Key: container, Size: 9, MTimeNS: 1234, Hash: "ownership-hash",
	}
	provider, source := newContainerSemanticProvider(
		container, fingerprint, parser.ParseOutcome{
			ResultSetComplete: true,
			ForceReplace:      true,
		},
	)
	provider.beforeParse = func() {
		require.NoError(t, database.CloseConnections())
	}
	engine := newSemanticTestEngine(t, database, root, provider)
	file := parser.DiscoveredFile{
		Path: container, Agent: parser.AgentOmnigent,
		ProviderSource: &source, ProviderProcess: true,
	}

	result := engine.processFile(t.Context(), file)

	require.NoError(t, database.Reopen())
	require.Error(t, result.err)
	assert.Empty(t, engine.SnapshotSkipCache())
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
	engine.forceFullParse = true
	forced := engine.dropUnchangedSharedSQLiteResults(
		file, []parser.ParseResult{result}, parser.UnchangedResultMTime,
	)

	assert.Empty(t, mtimeOnly)
	require.Len(t, mtimeAndHash, 1)
	assert.Equal(t, "semantic:member", mtimeAndHash[0].Session.ID)
	require.Len(t, forced, 1)
	assert.Equal(t, "semantic:member", forced[0].Session.ID)
}

func TestOmnigentDependentSourceExpansionPreservesEngineIDPrefixing(
	t *testing.T,
) {
	database := openTestDB(t)
	container := filepath.Join(t.TempDir(), "chat.db")
	rootPath := parser.VirtualSourcePath(container, "root")
	childPath := parser.VirtualSourcePath(container, "child")
	rootSource := parser.SourceRef{
		Provider:       parser.AgentOmnigent,
		Key:            rootPath,
		DisplayPath:    rootPath,
		FingerprintKey: rootPath,
	}
	childSource := parser.SourceRef{
		Provider:       parser.AgentOmnigent,
		Key:            childPath,
		DisplayPath:    childPath,
		FingerprintKey: childPath,
	}
	provider := &semanticTestProvider{
		Def: parser.AgentDef{Type: parser.AgentOmnigent, FileBased: true},
		reconciled: map[string]parser.SourceRef{
			childPath: childSource,
		},
	}
	parentID := "remote~omnigent:root"
	require.NoError(t, database.UpsertSession(db.Session{
		ID:       parentID,
		Agent:    string(parser.AgentOmnigent),
		Machine:  "",
		FilePath: &rootPath,
	}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID:              "remote~omnigent:child",
		Agent:           string(parser.AgentOmnigent),
		Machine:         "",
		ParentSessionID: &parentID,
		FilePath:        &childPath,
	}))
	engine := &Engine{
		db:       database,
		machine:  "local",
		idPrefix: "remote~",
	}

	expanded, err := engine.expandOmnigentInheritedMetadataSources(
		t.Context(), provider, []parser.SourceRef{rootSource},
	)

	require.NoError(t, err)
	assert.ElementsMatch(t, []parser.SourceRef{rootSource, childSource}, expanded)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = engine.expandOmnigentInheritedMetadataSources(
		canceled, provider, []parser.SourceRef{rootSource},
	)
	require.ErrorContains(t, err, "list omnigent parent session machines")
}

// TestBaselineFailureDoesNotPromoteSkipCache pins the omnigent-only cache
// deferral: a rowless or cache-after-write container entry may vouch for the
// source with no stored rows, so a failed ownership baseline must reject it
// instead of leaving the skip cache claiming freshness for state that never
// committed.
func TestBaselineFailureDoesNotPromoteSkipCache(t *testing.T) {
	for _, tc := range []struct {
		name          string
		seedUnchanged bool
		writtenResult bool
		nonStreamed   bool
	}{
		{name: "zero result"},
		{name: "unchanged result", seedUnchanged: true},
		{name: "non-streamed", nonStreamed: true},
		{name: "container", writtenResult: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.nonStreamed {
				testNonStreamedBaselineFailureRejectsCache(t)
				return
			}
			testReconciliationBaselineFailureRejectsCache(
				t, tc.seedUnchanged, tc.writtenResult,
			)
		})
	}
}

func testNonStreamedBaselineFailureRejectsCache(t *testing.T) {
	t.Helper()
	database := openTestDB(t)
	path := filepath.Join(t.TempDir(), "chat.db")
	engine := NewEngine(database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	results := make(chan syncJob, 1)
	results <- syncJob{
		agent:     parser.AgentOmnigent,
		path:      path,
		cacheSkip: true,
		cacheKey:  path + "?complete",
		mtime:     1234,
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

func testReconciliationBaselineFailureRejectsCache(
	t *testing.T,
	seedUnchanged bool,
	writtenResult bool,
) {
	t.Helper()
	database := openTestDB(t)
	root := t.TempDir()
	container := filepath.Join(root, "chat.db")
	require.NoError(t, os.WriteFile(container, []byte("container"), 0o600))
	fingerprint := parser.SourceFingerprint{
		Key: container, Size: 9, MTimeNS: 1234, Hash: "container-hash",
	}
	var results []parser.ParseResultOutcome
	forceReplace := false
	switch {
	case seedUnchanged:
		memberPath := container + "#unchanged"
		size := fingerprint.Size
		mtime := fingerprint.MTimeNS
		hash := fingerprint.Hash
		require.NoError(t, database.UpsertSession(db.Session{
			ID: "omnigent:unchanged", Agent: string(parser.AgentOmnigent),
			Machine: "devbox", Project: "semantic-project",
			FilePath: &memberPath, FileSize: &size, FileMtime: &mtime,
			FileHash: &hash,
		}))
		require.NoError(t, database.SetSessionDataVersion(
			"omnigent:unchanged", db.CurrentDataVersion(),
		))
		unchangedResult := semanticTestResult(
			"omnigent:unchanged", memberPath, fingerprint,
		)
		unchangedResult.Result.Session.File.Hash = fingerprint.Hash
		results = []parser.ParseResultOutcome{unchangedResult}
	case writtenResult:
		results = []parser.ParseResultOutcome{semanticTestResult(
			"omnigent:session", container+"#session", fingerprint,
		)}
		forceReplace = true
	}
	provider, _ := newContainerSemanticProvider(
		container, fingerprint, parser.ParseOutcome{
			Results:           results,
			ResultSetComplete: true,
			ForceReplace:      forceReplace,
		},
	)
	if seedUnchanged {
		provider.Caps.Sync.UnchangedResults = parser.UnchangedResultMTimeAndHash
	}
	engine := newSemanticTestEngine(t, database, root, provider)
	if writtenResult {
		engine.writeBatchOverride = func(
			batch []pendingWrite, _ syncWriteMode, _ bool,
		) (int, int, int, int) {
			return len(batch), 0, 0, 0
		}
	}
	require.NoError(t, database.CloseWriter())

	err := engine.ReconcileWatchRoots(t.Context(), []string{root}, true)

	require.Error(t, err)
	require.NoError(t, database.ReopenWriter())
	assert.Equal(t, 1, provider.parseCalls,
		"the regression must exercise a complete parsed outcome")
	assert.Empty(t, engine.SnapshotSkipCache(),
		"no-write cache freshness must wait for the ownership baseline")
}

// TestBaselineFailureKeepsRowlessCacheForOtherProviders pins the restored
// pre-omnigent timing: ordinary providers write rowless cache entries
// immediately and a failed ownership baseline does not revoke them.
func TestBaselineFailureKeepsRowlessCacheForOtherProviders(t *testing.T) {
	database := openTestDB(t)
	path := filepath.Join(t.TempDir(), "container.db")
	engine := NewEngine(database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	results := make(chan syncJob, 1)
	results <- syncJob{
		agent:     semanticTestAgent,
		path:      path,
		cacheSkip: true,
		cacheKey:  path + "?complete",
		mtime:     1234,
	}
	close(results)
	require.NoError(t, database.CloseWriter())

	stats := engine.collectAndBatch(
		t.Context(), results, 1, 1, nil, syncWriteDefault,
	)

	require.NoError(t, database.ReopenWriter())
	assert.Positive(t, stats.Failed)
	assert.Equal(t, map[string]int64{path + "?complete": 1234},
		engine.SnapshotSkipCache(),
		"non-omnigent rowless cache writes stay immediate")
}
