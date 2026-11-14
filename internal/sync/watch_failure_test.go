package sync

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type reconciliationRetryRootError interface {
	error
	ReconciliationRetryRoots() []string
}

func newFilesystemDiscoveryEngine(t *testing.T, root string) (*Engine, *db.DB) {
	t.Helper()
	const agent parser.AgentType = "filesystem-discovery"
	factory := parser.NewSourceSetFactory(
		parser.AgentDef{Type: agent, FileBased: true},
		parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources: parser.CapabilitySupported,
			WatchSources:    parser.CapabilitySupported,
			FindSource:      parser.CapabilitySupported,
		}},
		func(cfg parser.ProviderConfig) parser.SourceSet {
			return parser.NewJSONLSourceSet(agent, cfg.Roots,
				parser.WithRecursive(), parser.WithSymlinkFollowing(), parser.WithContentHashing(),
				parser.WithForceReplace(),
				parser.WithParseFile(func(ctx context.Context, path string, req parser.ParseRequest) ([]parser.ParseResult, []string, error) {
					if err := ctx.Err(); err != nil {
						return nil, nil, err
					}
					data, err := os.ReadFile(path)
					if err != nil {
						return nil, nil, err
					}
					var fixture struct {
						ID   string `json:"id"`
						Text string `json:"text"`
					}
					if err := json.Unmarshal(data, &fixture); err != nil {
						return nil, nil, err
					}
					started := time.Unix(1704067200, 0)
					return []parser.ParseResult{{
						Session: parser.ParsedSession{
							ID: fixture.ID, Agent: agent, Project: "fixture", Machine: req.Machine,
							StartedAt: started, EndedAt: started, FirstMessage: fixture.Text,
							MessageCount: 1, UserMessageCount: 1,
							File: parser.FileInfo{
								Path: path, Size: req.Fingerprint.Size,
								Mtime: req.Fingerprint.MTimeNS, Hash: req.Fingerprint.Hash,
							},
						},
						Messages: []parser.ParsedMessage{{Role: "user", Content: fixture.Text, Timestamp: started}},
					}}, nil, nil
				}),
			)
		},
	)
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
		ProviderFactories: []parser.ProviderFactory{factory},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			agent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	return engine, database
}

func partialDiscoveryRetry(t *testing.T, err error) WatchBatch {
	t.Helper()
	require.ErrorIs(t, err, os.ErrNotExist)
	retry, ok := callbackRetryBatch(err)
	require.True(t, ok, "inaccessible discovery must retain retry coverage")
	return retry
}

func partialDiscoverySessionContents(t *testing.T, database *db.DB, id string) []string {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	var contents []string
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return contents
}

type partialFailureStreamingProvider struct {
	*directStreamingProvider
	discoveryErr error
}

func (provider *partialFailureStreamingProvider) DiscoverEach(
	ctx context.Context, yield func(parser.SourceRef) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if provider.source != nil {
		if err := yield(*provider.source); err != nil {
			return err
		}
	}
	return provider.discoveryErr
}

type partialFailureStreamingFactory struct {
	provider *partialFailureStreamingProvider
}

func (factory partialFailureStreamingFactory) Definition() parser.AgentDef {
	return factory.provider.Definition()
}

func (factory partialFailureStreamingFactory) Capabilities() parser.Capabilities {
	return factory.provider.Capabilities()
}

type partialFailureStreamingScopedProvider struct {
	*partialFailureStreamingProvider
	scopes parser.ProviderBase
}

func (p partialFailureStreamingScopedProvider) ResolveReconciliationScopes(
	ctx context.Context, req parser.ReconciliationScopeRequest,
) (parser.ReconciliationScopePlan, error) {
	return p.scopes.ResolveReconciliationScopes(ctx, req)
}

func (factory partialFailureStreamingFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return partialFailureStreamingScopedProvider{
		partialFailureStreamingProvider: factory.provider,
		scopes:                          perCallScopeProviderBase(factory.provider.ProviderBase, cfg),
	}
}

type fingerprintCountingProvider struct {
	*directStreamingProvider
	fingerprintCalls int
}

type changedPathFailureProvider struct {
	parser.ProviderBase
	root              string
	watchPlanErr      error
	classificationErr error
	storedHintScopes  bool
	contextValue      any
	sourceCalls       int
}

func (p *changedPathFailureProvider) WatchPlan(
	context.Context,
) (parser.WatchPlan, error) {
	if p.watchPlanErr != nil {
		return parser.WatchPlan{}, p.watchPlanErr
	}
	return parser.WatchPlan{Roots: []parser.WatchRoot{{Path: p.root}}}, nil
}

func (p *changedPathFailureProvider) SourcesForChangedPath(
	ctx context.Context, _ parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	p.sourceCalls++
	p.contextValue = ctx.Value(changedPathContextKey{})
	return nil, p.classificationErr
}

func (p *changedPathFailureProvider) StoredSourceHintScopes(
	req parser.ChangedPathRequest,
) []parser.StoredSourceHintScope {
	if !p.storedHintScopes {
		return nil
	}
	return []parser.StoredSourceHintScope{{Path: req.Path}}
}

func (*changedPathFailureProvider) Parse(
	context.Context, parser.ParseRequest,
) (parser.ParseOutcome, error) {
	return parser.ParseOutcome{}, nil
}

type changedPathFailureFactory struct {
	provider *changedPathFailureProvider
}

func (f changedPathFailureFactory) Definition() parser.AgentDef {
	return f.provider.Definition()
}

func (f changedPathFailureFactory) Capabilities() parser.Capabilities {
	return f.provider.Capabilities()
}

type changedPathFailureScopedProvider struct {
	*changedPathFailureProvider
	scopes parser.ProviderBase
}

func (p changedPathFailureScopedProvider) ResolveReconciliationScopes(
	ctx context.Context, req parser.ReconciliationScopeRequest,
) (parser.ReconciliationScopePlan, error) {
	return p.scopes.ResolveReconciliationScopes(ctx, req)
}

func (f changedPathFailureFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return changedPathFailureScopedProvider{
		changedPathFailureProvider: f.provider,
		scopes:                     perCallScopeProviderBase(f.provider.ProviderBase, cfg),
	}
}

type changedPathContextKey struct{}

func newChangedPathFailureEngine(
	t *testing.T,
	database *db.DB,
	root string,
	provider *changedPathFailureProvider,
) *Engine {
	t.Helper()
	provider.root = root
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{"changed-path-failure": {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			changedPathFailureFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			"changed-path-failure": parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	return engine
}

func (p *fingerprintCountingProvider) Fingerprint(
	context.Context, parser.SourceRef,
) (parser.SourceFingerprint, error) {
	p.fingerprintCalls++
	return parser.SourceFingerprint{Hash: "stored-hash"}, nil
}

func TestSyncPathsContextPropagatesChangedPathClassificationFailure(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	changedPath := filepath.Join(root, "replacement.jsonl")
	missingPath := filepath.Join(root, "previous.jsonl")
	require.NoError(t, os.WriteFile(changedPath, []byte("{}\n"), 0o600))
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "previous", Agent: "changed-path-failure", Project: "project",
		Machine: "local", FilePath: &missingPath,
	}))
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "local", []db.SessionSourcePath{{
			Agent: "changed-path-failure", FilePath: missingPath,
		}},
	))
	wantErr := errors.New("changed-path classification failed")
	provider := &changedPathFailureProvider{
		Def:               parser.AgentDef{Type: "changed-path-failure", FileBased: true},
		classificationErr: wantErr,
	}
	engine := newChangedPathFailureEngine(t, database, root, provider)
	ctx := context.WithValue(t.Context(), changedPathContextKey{}, "caller")

	err := engine.SyncPathsContext(ctx, []string{changedPath, missingPath})

	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, "caller", provider.contextValue,
		"changed-path classification must receive the watcher context")
	stored, getErr := database.GetSession(t.Context(), "previous")
	require.NoError(t, getErr)
	assert.NotNil(t, stored,
		"a classification failure must suppress missing-source tombstones")
}

func TestSyncPathsContextPropagatesChangedPathWatchRootFailure(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	changedPath := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(changedPath, []byte("{}\n"), 0o600))
	wantErr := errors.New("watch root resolution failed")
	provider := &changedPathFailureProvider{
		Def:          parser.AgentDef{Type: "changed-path-failure", FileBased: true},
		watchPlanErr: wantErr,
	}
	engine := newChangedPathFailureEngine(t, database, root, provider)

	err := engine.SyncPathsContext(t.Context(), []string{changedPath})

	require.ErrorIs(t, err, wantErr)
	assert.Zero(t, provider.sourceCalls,
		"classification must stop when its watch-root plan is unresolved")
}

func TestSyncPathsContextPropagatesStoredHintCancellation(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	changedPath := filepath.Join(root, "container.db")
	require.NoError(t, os.WriteFile(changedPath, []byte("fixture"), 0o600))
	provider := &changedPathFailureProvider{
		Def: parser.AgentDef{Type: "changed-path-failure", FileBased: true},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			StoredSourceHints: parser.CapabilitySupported,
		}},
		storedHintScopes: true,
	}
	engine := newChangedPathFailureEngine(t, database, root, provider)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := engine.SyncPathsContext(ctx, []string{changedPath})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, provider.sourceCalls,
		"provider classification must not run without its requested stored hints")
}

func TestSyncPathsContextDoesNotTombstoneAfterIncompleteReplacement(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	changedPath := filepath.Join(root, "replacement.jsonl")
	missingPath := filepath.Join(root, "previous.jsonl")
	require.NoError(t, os.WriteFile(changedPath, []byte("{}\n"), 0o600))
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "previous", Agent: "watch-failure", Project: "project",
		Machine: "local", FilePath: &missingPath,
	}))
	source := parser.SourceRef{
		Provider: "watch-failure", Key: changedPath,
		DisplayPath: changedPath, FingerprintKey: changedPath,
	}
	provider := &directStreamingProvider{
		Def: parser.AgentDef{Type: "watch-failure", FileBased: true},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources: parser.CapabilitySupported,
			WatchSources:    parser.CapabilitySupported,
			FindSource:      parser.CapabilitySupported,
		}},
		source:   &source,
		parseErr: errors.New("replacement parse failed"),
	}
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{"watch-failure": {root}},
		Machine:   "local",
		ProviderFactories: []parser.ProviderFactory{
			directStreamingFactory{provider: provider},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			"watch-failure": parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	err := engine.SyncPathsContext(t.Context(), []string{changedPath, missingPath})

	require.Error(t, err)
	require.ErrorContains(t, err, "incomplete")
	stored, getErr := database.GetSession(t.Context(), "previous")
	require.NoError(t, getErr)
	assert.NotNil(t, stored,
		"a failed replacement must not hide the previous archived session")
}

func TestReconcileWatchRootsCommitsHealthyProvidersAndScopesFailedRetry(t *testing.T) {
	database := openTestDB(t)
	healthyRoot := t.TempDir()
	failedRoot := t.TempDir()
	healthyPath := filepath.Join(healthyRoot, "session.jsonl")
	healthyMissingPath := filepath.Join(healthyRoot, "removed.jsonl")
	require.NoError(t, os.WriteFile(healthyPath, []byte("{}\n"), 0o600))
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "healthy-removed", Agent: "healthy-stream", Project: "project",
		Machine: "local", FilePath: &healthyMissingPath,
	}))
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "local", []db.SessionSourcePath{{
			Agent: "healthy-stream", FilePath: healthyMissingPath,
		}},
	))
	healthySource := parser.SourceRef{
		Provider: "healthy-stream", Key: healthyPath,
		DisplayPath: healthyPath, FingerprintKey: healthyPath,
	}
	started := time.Unix(1704067200, 0)
	healthy := &directStreamingProvider{
		Def: parser.AgentDef{Type: "healthy-stream", FileBased: true},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources:    parser.CapabilitySupported,
			StreamingDiscovery: parser.CapabilitySupported,
			WatchSources:       parser.CapabilitySupported,
			FindSource:         parser.CapabilitySupported,
		}},
		source: &healthySource,
		parseOutcome: parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{{
				Result: parser.ParseResult{Session: parser.ParsedSession{
					ID: "healthy-session", Agent: "healthy-stream",
					Project: "project", Machine: "local",
					StartedAt: started, EndedAt: started,
					File: parser.FileInfo{Path: healthyPath},
				}},
				DataVersion: parser.DataVersionCurrent,
			}},
			ResultSetComplete: true,
		},
	}
	failed := &failingDBBackedProvider{
		Def: parser.AgentDef{
			Type: "failed-stream", FileBased: true,
		},
		err: errors.New("permission denied"), failOnCall: 1,
	}
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			"healthy-stream": {healthyRoot},
			"failed-stream":  {failedRoot},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{
			directStreamingFactory{provider: healthy},
			failingDBBackedFactory{provider: failed},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			"healthy-stream": parser.ProviderMigrationProviderAuthoritative,
			"failed-stream":  parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	// Seed a pending subagent relationship among already-committed sessions:
	// the global linking pass must still run when only an unrelated provider's
	// discovery failed, or the relationship stays missing indefinitely.
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "link-parent", Agent: "healthy-stream", Project: "project",
		Machine: "local",
	}))
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "link-child", Agent: "healthy-stream", Project: "project",
		Machine: "local",
	}))
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{
		SessionID: "link-parent", Ordinal: 0, Role: "assistant",
		Content: "spawning subagent", HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "subagent", Category: "Task",
			SubagentSessionID: "link-child",
		}},
	}}))

	err := engine.ReconcileWatchRoots(t.Context(), nil, true)

	require.Error(t, err)
	stored, getErr := database.GetSession(t.Context(), "healthy-session")
	require.NoError(t, getErr)
	assert.NotNil(t, stored,
		"one provider failure must not discard healthy provider candidates")
	linked, getErr := database.GetSession(t.Context(), "link-child")
	require.NoError(t, getErr)
	require.NotNil(t, linked)
	assert.Equal(t, "subagent", linked.RelationshipType,
		"partial provider failure must not suppress the global subagent linking pass")
	if assert.NotNil(t, linked.ParentSessionID) {
		assert.Equal(t, "link-parent", *linked.ParentSessionID)
	}
	removed, getErr := database.GetSessionFull(t.Context(), "healthy-removed")
	require.NoError(t, getErr)
	assertSourceMissingState(t, removed)
	var retryErr reconciliationRetryRootError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, []string{failedRoot}, retryErr.ReconciliationRetryRoots())
}

// TestReconcileWatchRootsLinkingFailureExpandsRetryToCompletedScopes pins the
// combined-failure retry scope: when the deferred subagent-linking pass fails
// alongside a provider discovery failure, the completed healthy scopes are not
// tombstoned, so they must join the retry roots instead of staying stale
// indefinitely behind a retry that only re-runs the failed provider.
func TestReconcileWatchRootsLinkingFailureExpandsRetryToCompletedScopes(t *testing.T) {
	database := openTestDB(t)
	healthyRoot := t.TempDir()
	failedRoot := t.TempDir()
	healthyPath := filepath.Join(healthyRoot, "session.jsonl")
	require.NoError(t, os.WriteFile(healthyPath, []byte("{}\n"), 0o600))
	healthySource := parser.SourceRef{
		Provider: "healthy-stream", Key: healthyPath,
		DisplayPath: healthyPath, FingerprintKey: healthyPath,
	}
	started := time.Unix(1704067200, 0)
	healthy := &directStreamingProvider{
		Def: parser.AgentDef{Type: "healthy-stream", FileBased: true},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			DiscoverSources:    parser.CapabilitySupported,
			StreamingDiscovery: parser.CapabilitySupported,
			WatchSources:       parser.CapabilitySupported,
			FindSource:         parser.CapabilitySupported,
		}},
		source: &healthySource,
		parseOutcome: parser.ParseOutcome{
			Results: []parser.ParseResultOutcome{{
				Result: parser.ParseResult{Session: parser.ParsedSession{
					ID: "healthy-session", Agent: "healthy-stream",
					Project: "project", Machine: "local",
					StartedAt: started, EndedAt: started,
					File: parser.FileInfo{Path: healthyPath},
				}},
				DataVersion: parser.DataVersionCurrent,
			}},
			ResultSetComplete: true,
		},
	}
	failed := &failingDBBackedProvider{
		Def: parser.AgentDef{
			Type: "failed-stream", FileBased: true,
		},
		err: errors.New("permission denied"), failOnCall: 1,
	}
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			"healthy-stream": {healthyRoot},
			"failed-stream":  {failedRoot},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{
			directStreamingFactory{provider: healthy},
			failingDBBackedFactory{provider: failed},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			"healthy-stream": parser.ProviderMigrationProviderAuthoritative,
			"failed-stream":  parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	// Seed a pending link so the global linking pass performs an update, and
	// make that update fail: only linking transitions relationship_type to
	// "subagent", so page writes are unaffected.
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "link-parent", Agent: "healthy-stream", Project: "project",
		Machine: "local",
	}))
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "link-child", Agent: "healthy-stream", Project: "project",
		Machine: "local",
	}))
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{
		SessionID: "link-parent", Ordinal: 0, Role: "assistant",
		Content: "spawning subagent", HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "subagent", Category: "Task",
			SubagentSessionID: "link-child",
		}},
	}}))
	raw, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.ExecContext(t.Context(), `
		CREATE TRIGGER fail_subagent_link
		BEFORE UPDATE OF relationship_type ON sessions
		WHEN NEW.relationship_type = 'subagent'
		BEGIN
			SELECT RAISE(FAIL, 'injected linking failure');
		END;
	`)
	require.NoError(t, err)

	err = engine.ReconcileWatchRoots(t.Context(), nil, true)

	require.Error(t, err)
	require.ErrorContains(t, err, "link subagent sessions")
	var retryErr reconciliationRetryRootError
	require.ErrorAs(t, err, &retryErr)
	assert.ElementsMatch(t, []string{failedRoot, healthyRoot},
		retryErr.ReconciliationRetryRoots(),
		"a linking failure blocks completed-scope tombstoning, so those "+
			"scopes must join the retry roots")
	stored, getErr := database.GetSession(t.Context(), "healthy-session")
	require.NoError(t, getErr)
	assert.NotNil(t, stored,
		"the linking failure must not discard committed healthy sessions")
}

func TestReconcileWatchRootsRetainsPartialFailedProviderWithoutDeletionProof(t *testing.T) {
	database := openTestDB(t)
	failedRoot := t.TempDir()
	healthyRoot := t.TempDir()
	failedPath := filepath.Join(failedRoot, "partial.jsonl")
	failedMissingPath := filepath.Join(failedRoot, "missing.jsonl")
	healthyPath := filepath.Join(healthyRoot, "session.jsonl")
	healthyMissingPath := filepath.Join(healthyRoot, "missing.jsonl")
	for _, path := range []string{failedPath, healthyPath} {
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	}
	for _, session := range []db.Session{
		{
			ID: "failed-missing", Agent: "partial-failure", Project: "project",
			Machine: "local", FilePath: &failedMissingPath,
		},
		{
			ID: "healthy-missing", Agent: "partial-healthy", Project: "project",
			Machine: "local", FilePath: &healthyMissingPath,
		},
	} {
		require.NoError(t, database.UpsertSession(t.Context(), session))
	}
	require.NoError(t, database.BaselineActiveSessionSourcePaths(
		t.Context(), "local", []db.SessionSourcePath{
			{Agent: "partial-failure", FilePath: failedMissingPath},
			{Agent: "partial-healthy", FilePath: healthyMissingPath},
		},
	))

	started := time.Unix(1704067200, 0)
	newProvider := func(agent parser.AgentType, path, id string) *directStreamingProvider {
		source := parser.SourceRef{
			Provider: agent, Key: path, DisplayPath: path, FingerprintKey: path,
		}
		return &directStreamingProvider{
			Def: parser.AgentDef{Type: agent, FileBased: true},
			Caps: parser.Capabilities{Source: parser.SourceCapabilities{
				DiscoverSources:    parser.CapabilitySupported,
				StreamingDiscovery: parser.CapabilitySupported,
				WatchSources:       parser.CapabilitySupported,
				FindSource:         parser.CapabilitySupported,
			}},
			source: &source,
			parseOutcome: parser.ParseOutcome{
				Results: []parser.ParseResultOutcome{{
					Result: parser.ParseResult{Session: parser.ParsedSession{
						ID: id, Agent: agent, Project: "project", Machine: "local",
						StartedAt: started, EndedAt: started,
						File: parser.FileInfo{Path: path},
					}},
					DataVersion: parser.DataVersionCurrent,
				}},
				ResultSetComplete: true,
			},
		}
	}
	discoveryErr := errors.New("partial provider discovery failed")
	failed := &partialFailureStreamingProvider{
		directStreamingProvider: newProvider("partial-failure", failedPath, "partial-session"),
		discoveryErr:            discoveryErr,
	}
	healthy := newProvider("partial-healthy", healthyPath, "healthy-session")
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			"partial-failure": {failedRoot},
			"partial-healthy": {healthyRoot},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{
			partialFailureStreamingFactory{provider: failed},
			directStreamingFactory{provider: healthy},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			"partial-failure": parser.ProviderMigrationProviderAuthoritative,
			"partial-healthy": parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	err := engine.ReconcileWatchRoots(t.Context(), nil, true)

	require.ErrorIs(t, err, discoveryErr)
	var retryErr reconciliationRetryRootError
	require.ErrorAs(t, err, &retryErr)
	assert.Equal(t, []string{failedRoot}, retryErr.ReconciliationRetryRoots())
	partial, getErr := database.GetSession(t.Context(), "partial-session")
	require.NoError(t, getErr)
	assert.NotNil(t, partial, "a valid source yielded before discovery failure must persist")
	failedMissing, getErr := database.GetSessionFull(t.Context(), "failed-missing")
	require.NoError(t, getErr)
	require.NotNil(t, failedMissing)
	assert.Nil(t, failedMissing.DeletionCause,
		"an incomplete provider scope must not tombstone missing sources")
	failedOwnership, listErr := database.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), "local", "partial-failure",
		[]db.StoredSourcePathHintScope{{Path: failedRoot}}, db.SessionSourceCursor{},
	)
	require.NoError(t, listErr)
	require.Len(t, failedOwnership, 1,
		"an incomplete provider scope must neither add nor remove deletion proof")
	assert.Equal(t, failedMissingPath, failedOwnership[0].FilePath)

	healthyStored, getErr := database.GetSession(t.Context(), "healthy-session")
	require.NoError(t, getErr)
	assert.NotNil(t, healthyStored)
	healthyMissing, getErr := database.GetSessionFull(t.Context(), "healthy-missing")
	require.NoError(t, getErr)
	assertSourceMissingState(t, healthyMissing)
	healthyOwnership, listErr := database.ListActiveSessionSourceOwnershipScopesPage(
		t.Context(), "local", "partial-healthy",
		[]db.StoredSourcePathHintScope{{Path: healthyRoot}}, db.SessionSourceCursor{},
	)
	require.NoError(t, listErr)
	require.Len(t, healthyOwnership, 1)
	assert.Equal(t, healthyPath, healthyOwnership[0].FilePath,
		"an independent completed scope must baseline its admitted source")
}

func TestWatchDiscoveryFailurePreservesSessionsAndNarrowsRetry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions#branch")
	require.NoError(t, os.Mkdir(root, 0o755))
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.Mkdir(target, 0o755))
	link := filepath.Join(root, "linked#branch")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	linkedPath := filepath.Join(target, "linked.jsonl")
	healthyPath := filepath.Join(root, "healthy.jsonl")
	require.NoError(t, os.WriteFile(linkedPath, []byte(
		`{"id":"linked","text":"linked before"}`,
	), 0o600))
	require.NoError(t, os.WriteFile(healthyPath, []byte(
		`{"id":"healthy","text":"healthy before"}`,
	), 0o600))
	removedPath := link + "#removed.jsonl"
	require.NoError(t, os.WriteFile(removedPath, []byte(`{"id":"removed","text":"removed source"}`), 0o600))
	engine, database := newFilesystemDiscoveryEngine(t, root)
	require.NoError(t, ApplyWatchBatch(t.Context(), engine, WatchBatch{ReconcileRoots: []string{root}}, nil))
	before, err := database.GetSessionFull(t.Context(), "linked")
	require.NoError(t, err)
	require.NotNil(t, before)
	assert.Equal(t, []string{"linked before"}, partialDiscoverySessionContents(t, database, "linked"))

	parked := target + "-unavailable"
	require.NoError(t, os.Remove(removedPath))
	require.NoError(t, os.Rename(target, parked))
	recovery := &WatchRecoveryScope{AvailableRoots: []string{root}}
	err = ApplyWatchBatch(t.Context(), engine, WatchBatch{FullSync: true, LostEvents: true}, recovery)
	retry := partialDiscoveryRetry(t, err)
	assert.EqualExportedValues(t, WatchBatch{ReconcileRoots: []string{link}, LostEvents: true}, retry,
		"a failed subtree must not retain the healthy sibling in its retry scope")
	removed, err := database.GetSessionFull(t.Context(), "removed")
	require.NoError(t, err)
	assertSourceMissingState(t, removed)

	require.NoError(t, os.WriteFile(healthyPath, []byte(
		`{"id":"healthy","text":"healthy after"}`,
	), 0o600))
	require.NoError(t, ApplyWatchBatch(t.Context(), engine, WatchBatch{Paths: []string{healthyPath}}, nil))
	assert.Equal(t, []string{"healthy after"}, partialDiscoverySessionContents(t, database, "healthy"))
	err = ApplyWatchBatch(t.Context(), engine, retry, recovery)
	retry = partialDiscoveryRetry(t, err)
	preserved, err := database.GetSessionFull(t.Context(), "linked")
	require.NoError(t, err)
	assert.Equal(t, before, preserved, "failed access must preserve the complete archived session")
	assert.Equal(t, []string{"linked before"}, partialDiscoverySessionContents(t, database, "linked"))

	require.NoError(t, os.WriteFile(filepath.Join(parked, "linked.jsonl"), []byte(
		`{"id":"linked","text":"linked after"}`,
	), 0o600))
	require.NoError(t, os.Rename(parked, target))
	require.NoError(t, ApplyWatchBatch(t.Context(), engine, retry, recovery))
	assert.Equal(t, []string{"linked after"}, partialDiscoverySessionContents(t, database, "linked"))
	restored, err := database.GetSessionFull(t.Context(), "linked")
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Equal(t, new(filepath.Join(link, "linked.jsonl")), restored.FilePath)

	require.NoError(t, os.Remove(linkedPath))
	require.NoError(t, ApplyWatchBatch(t.Context(), engine, WatchBatch{ReconcileRoots: []string{link}}, nil))
	missing, err := database.GetSessionFull(t.Context(), "linked")
	require.NoError(t, err)
	assertSourceMissingState(t, missing)
}

func TestWatchDiscoveryFailurePreservesSessionsWithRelativeRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	root := filepath.Join("sources", "sessions")
	require.NoError(t, os.MkdirAll(root, 0o755))
	target := t.TempDir()
	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	linkedFile := filepath.Join(target, "linked.jsonl")
	require.NoError(t, os.WriteFile(linkedFile, []byte(
		`{"id":"linked","text":"retained transcript"}`,
	), 0o600))
	removed := filepath.Join(root, "removed.jsonl")
	require.NoError(t, os.WriteFile(removed, []byte(
		`{"id":"removed","text":"removed source"}`,
	), 0o600))
	engine, database := newFilesystemDiscoveryEngine(t, root)
	require.NoError(t, ApplyWatchBatch(t.Context(), engine, WatchBatch{ReconcileRoots: []string{root}}, nil))
	before, err := database.GetSessionFull(t.Context(), "linked")
	require.NoError(t, err)
	require.NotNil(t, before)
	assert.Equal(t, new(filepath.Join(link, "linked.jsonl")), before.FilePath)

	parked := target + "-unavailable"
	require.NoError(t, os.Rename(target, parked))
	require.NoError(t, os.Remove(removed))
	recovery := &WatchRecoveryScope{AvailableRoots: []string{root}}
	err = ApplyWatchBatch(t.Context(), engine, WatchBatch{FullSync: true, LostEvents: true}, recovery)
	retry := partialDiscoveryRetry(t, err)
	preserved, err := database.GetSessionFull(t.Context(), "linked")
	require.NoError(t, err)
	assert.Equal(t, before, preserved, "a dangling relative-root symlink must not mark archived descendants source-missing")
	assert.Equal(t, []string{"retained transcript"}, partialDiscoverySessionContents(t, database, "linked"))
	missing, err := database.GetSessionFull(t.Context(), "removed")
	require.NoError(t, err)
	assertSourceMissingState(t, missing)

	require.NoError(t, os.Rename(parked, target))
	require.NoError(t, ApplyWatchBatch(t.Context(), engine, retry, recovery))
	restored, err := database.GetSessionFull(t.Context(), "linked")
	require.NoError(t, err)
	require.NotNil(t, restored)
	assert.Equal(t, new(filepath.Join(link, "linked.jsonl")), restored.FilePath)
	assert.Equal(t, []string{"retained transcript"}, partialDiscoverySessionContents(t, database, "linked"))
}

func TestWatchDiscoveryFailureRetryDoesNotTraverseHealthySiblings(t *testing.T) {
	for _, healthySources := range []int{1, 64} {
		for _, previouslyImported := range []bool{false, true} {
			t.Run(fmt.Sprintf("healthy=%d/imported=%t", healthySources, previouslyImported), func(t *testing.T) {
				root := t.TempDir()
				target := filepath.Join(t.TempDir(), "target")
				require.NoError(t, os.Mkdir(target, 0o755))
				link := filepath.Join(root, "linked")
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlink not supported: %v", err)
				}
				if previouslyImported {
					require.NoError(t, os.WriteFile(filepath.Join(target, "linked.jsonl"), []byte(
						`{"id":"linked","text":"preserved"}`,
					), 0o600))
				}
				healthy := filepath.Join(root, "healthy")
				require.NoError(t, os.Mkdir(healthy, 0o755))
				for i := range healthySources {
					path := filepath.Join(healthy, fmt.Sprintf("healthy-%02d.jsonl", i))
					body := fmt.Sprintf(`{"id":"healthy-%02d","text":"healthy"}`, i)
					require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
				}
				engine, database := newFilesystemDiscoveryEngine(t, root)
				require.NoError(t, ApplyWatchBatch(t.Context(), engine, WatchBatch{ReconcileRoots: []string{root}}, nil))
				require.NoError(t, os.Rename(target, target+"-unavailable"))
				recovery := &WatchRecoveryScope{AvailableRoots: []string{root}}
				err := ApplyWatchBatch(t.Context(), engine, WatchBatch{FullSync: true, LostEvents: true}, recovery)
				retry := partialDiscoveryRetry(t, err)

				siblingTarget := filepath.Join(t.TempDir(), "target")
				require.NoError(t, os.Mkdir(siblingTarget, 0o755))
				siblingLink := filepath.Join(healthy, "unavailable")
				require.NoError(t, os.Symlink(siblingTarget, siblingLink))
				require.NoError(t, os.Rename(siblingTarget, siblingTarget+"-unavailable"))
				require.NoError(t, os.WriteFile(filepath.Join(healthy, "new.jsonl"), []byte(
					`{"id":"new","text":"fresh sibling"}`,
				), 0o600))
				for range 3 {
					err = ApplyWatchBatch(t.Context(), engine, retry, recovery)
					retry = partialDiscoveryRetry(t, err)
					assert.EqualExportedValues(t, WatchBatch{ReconcileRoots: []string{link}, LostEvents: true}, retry)
					unrequested, getErr := database.GetSession(t.Context(), "new")
					require.NoError(t, getErr)
					assert.Nil(t, unrequested, "a retry must not import newly created sessions outside its scope")
				}
				require.NoError(t, os.Rename(target+"-unavailable", target))
				require.NoError(t, ApplyWatchBatch(t.Context(), engine, retry, recovery),
					"the original retry must complete despite an unrelated inaccessible sibling")
				unrequested, err := database.GetSession(t.Context(), "new")
				require.NoError(t, err)
				assert.Nil(t, unrequested)

				err = ApplyWatchBatch(t.Context(), engine, WatchBatch{ReconcileRoots: []string{healthy}}, nil)
				siblingRetry := partialDiscoveryRetry(t, err)
				assert.Equal(t, WatchBatch{ReconcileRoots: []string{siblingLink}}, siblingRetry)
				assert.Equal(t, []string{"fresh sibling"}, partialDiscoverySessionContents(t, database, "new"))
				require.NoError(t, os.Rename(siblingTarget+"-unavailable", siblingTarget))
				require.NoError(t, ApplyWatchBatch(t.Context(), engine, siblingRetry, nil))
			})
		}
	}
}

func TestWatchDiscoveryRetryPreservesCompletedFreshness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		preserve bool
	}{
		{"watcher retry", true},
		{"accumulated retry", true},
		{"fresh loss", false},
		{"transport", false},
		{"replacement engine", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), filepath.Join(root, "broken")); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}
			engine, _ := newFilesystemDiscoveryEngine(t, root)
			recovery := &WatchRecoveryScope{AvailableRoots: []string{root}}
			err := ApplyWatchBatch(t.Context(), engine, WatchBatch{FullSync: true, LostEvents: true}, recovery)
			retry := partialDiscoveryRetry(t, err)
			if tc.name == "replacement engine" {
				engine, _ = newFilesystemDiscoveryEngine(t, root)
			}
			key := filepath.Join(t.TempDir(), "previously-verified.jsonl")
			engine.InjectSkipCache(map[string]int64{key: 1})
			before := engine.SnapshotSkipCache()
			switch tc.name {
			case "watcher retry":
				sink := newWatchEventSink(defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes)
				sink.RetainRetry(retry)
				var ok bool
				retry, ok = sink.Take(nil)
				require.True(t, ok)
			case "accumulated retry", "fresh loss":
				accumulator := NewWatchBatchAccumulator(nil)
				accumulator.Add(retry)
				if tc.name == "fresh loss" {
					accumulator.Add(WatchBatch{FullSync: true, LostEvents: true})
				}
				var ok bool
				retry, ok = accumulator.Take()
				require.True(t, ok)
			case "transport":
				encoded, err := json.Marshal(retry)
				require.NoError(t, err)
				retry = WatchBatch{}
				require.NoError(t, json.Unmarshal(encoded, &retry))
			}
			err = ApplyWatchBatch(t.Context(), engine, retry, recovery)
			partialDiscoveryRetry(t, err)
			want := map[string]int64{}
			if tc.preserve {
				want = before
			}
			assert.Equal(t, want, engine.SnapshotSkipCache())
		})
	}
}

func TestReconcileWatchRootsJoinsProviderAndLaterProcessingFailures(t *testing.T) {
	database := openTestDB(t)
	failedRoot := t.TempDir()
	healthyRoot := t.TempDir()
	healthyPath := filepath.Join(healthyRoot, "session.jsonl")
	require.NoError(t, os.WriteFile(healthyPath, []byte("{}\n"), 0o600))
	discoveryErr := errors.New("provider discovery failed")
	laterErr := errors.New("reconciliation page failed")
	failed := &failingDBBackedProvider{
		Def: parser.AgentDef{
			Type: "join-failed", FileBased: true,
		},
		err: discoveryErr, failOnCall: 1,
	}
	healthySource := parser.SourceRef{
		Provider: "join-healthy", Key: healthyPath,
		DisplayPath: healthyPath, FingerprintKey: healthyPath,
	}
	healthy := &directStreamingProvider{
		Def: parser.AgentDef{Type: "join-healthy", FileBased: true},
		Caps: parser.Capabilities{Source: parser.SourceCapabilities{
			StreamingDiscovery: parser.CapabilitySupported,
		}},
		source: &healthySource,
	}
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			"join-failed":  {failedRoot},
			"join-healthy": {healthyRoot},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{
			failingDBBackedFactory{provider: failed},
			directStreamingFactory{provider: healthy},
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			"join-failed":  parser.ProviderMigrationProviderAuthoritative,
			"join-healthy": parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	engine.reconciliationSpoolFactory = func(ctx context.Context, path string) (reconciliationSpoolStore, error) {
		spool, err := newReconciliationSpool(ctx, path)
		if err != nil {
			return nil, err
		}
		return &failingReconciliationSpool{
			reconciliationSpoolStore: spool,
			err:                      laterErr,
		}, nil
	}

	err := engine.ReconcileWatchRoots(t.Context(), nil, true)

	require.ErrorIs(t, err, discoveryErr)
	require.ErrorIs(t, err, laterErr)
	var retryErr reconciliationRetryRootError
	require.ErrorAs(t, err, &retryErr)
	assert.ElementsMatch(t, []string{failedRoot, healthyRoot},
		retryErr.ReconciliationRetryRoots(),
		"a later global processing failure must retry every uncommitted provider scope")
}

func TestReconciliationCandidateDoesNotHashStableClaudeSource(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "stable-session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	hash := "stored-hash"
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "stable-session", Agent: string(parser.AgentClaude),
		Project: "project", Machine: "local", FilePath: &path,
		FileSize: &size, FileMtime: &mtime, FileHash: &hash,
		DataVersion: db.CurrentDataVersion(),
	}))
	require.NoError(t, database.SetSessionDataVersion(t.Context(),
		"stable-session", db.CurrentDataVersion(),
	))
	storedSize, storedMtime, stored := database.GetSessionFileInfo(t.Context(), "stable-session")
	require.True(t, stored)
	assert.Equal(t, size, storedSize)
	assert.Equal(t, mtime, storedMtime)
	assert.Equal(t, db.CurrentDataVersion(), database.GetSessionDataVersion(t.Context(), "stable-session"))
	base := &directStreamingProvider{
		Def: parser.AgentDef{Type: parser.AgentClaude, FileBased: true},
	}
	provider := &fingerprintCountingProvider{directStreamingProvider: base}
	engine := &Engine{db: database}
	source := parser.SourceRef{
		Provider: parser.AgentClaude, Key: path,
		DisplayPath: path, FingerprintKey: path,
	}

	_, admitted := engine.reconciliationCandidate(t.Context(),
		provider, source, []string{root}, nil,
	)

	assert.True(t, admitted)
	assert.Zero(t, provider.fingerprintCalls,
		"duplicate preference must not hash every unchanged Claude transcript")
}
