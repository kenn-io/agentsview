package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

const cursorRetryTestAgent parser.AgentType = "cursor-retry-test"

type cursorRetryFactory struct {
	mu                gosync.Mutex
	agent             parser.AgentType
	root              string
	source            parser.SourceRef
	forceConfigs      []bool
	discoveryCalls    int
	parseCalls        int
	dependentRootID   string
	discoveryErr      error
	reconciliationErr error
	rehydrationErr    error
	cancelAfterYield  context.CancelFunc
	cacheAfterWrite   bool
	sources           []parser.SourceRef
	parseErrors       map[string]error
}

func (f *cursorRetryFactory) Definition() parser.AgentDef {
	agent := f.agent
	idPrefix := string(agent) + ":"
	if agent == "" {
		agent = cursorRetryTestAgent
		idPrefix = "cursor-retry:"
	}
	return parser.AgentDef{
		Type: agent, IDPrefix: idPrefix,
		FileBased: true,
	}
}

func (f *cursorRetryFactory) Capabilities() parser.Capabilities {
	return parser.Capabilities{
		Source: parser.SourceCapabilities{
			DiscoverSources:            parser.CapabilitySupported,
			StreamingDiscovery:         parser.CapabilitySupported,
			IncrementalDiscoveryCursor: parser.CapabilitySupported,
			WatchSources:               parser.CapabilitySupported,
			FindSource:                 parser.CapabilitySupported,
			MultiSessionSource:         parser.CapabilitySupported,
		},
		Sync: parser.ProviderSyncSemantics{
			FingerprintHashInCacheKey: true,
		},
	}
}

func (f *cursorRetryFactory) NewProvider(
	cfg parser.ProviderConfig,
) parser.Provider {
	return &cursorRetryProvider{
		ProviderBase: parser.ProviderBase{
			Def: f.Definition(), Caps: f.Capabilities(), Config: cfg,
		},
		factory: f,
		cfg:     cfg,
	}
}

func (f *cursorRetryFactory) snapshot() ([]bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.forceConfigs...), f.parseCalls
}

type cursorRetryProvider struct {
	parser.ProviderBase
	factory *cursorRetryFactory
	cfg     parser.ProviderConfig
}

func (p *cursorRetryProvider) Definition() parser.AgentDef {
	return p.factory.Definition()
}

func (p *cursorRetryProvider) Capabilities() parser.Capabilities {
	return p.factory.Capabilities()
}

func (p *cursorRetryProvider) Discover(
	context.Context,
) ([]parser.SourceRef, error) {
	return nil, errors.New("slice discovery is not allowed")
}

func (p *cursorRetryProvider) DiscoverEach(
	ctx context.Context,
	yield func(parser.SourceRef) error,
) error {
	p.factory.mu.Lock()
	p.factory.discoveryCalls++
	call := p.factory.discoveryCalls
	p.factory.forceConfigs = append(
		p.factory.forceConfigs, p.cfg.ForceFullDiscovery,
	)
	sources := append([]parser.SourceRef(nil), p.factory.sources...)
	if len(sources) == 0 {
		sources = []parser.SourceRef{p.factory.source}
	}
	p.factory.mu.Unlock()
	if call != 1 && !p.cfg.ForceFullDiscovery {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, source := range sources {
		err := yield(source)
		if err != nil {
			return err
		}
		if p.factory.cancelAfterYield != nil {
			p.factory.cancelAfterYield()
		}
	}
	if p.factory.discoveryErr != nil {
		return p.factory.discoveryErr
	}
	return nil
}

func (p *cursorRetryProvider) WatchPlan(
	context.Context,
) (parser.WatchPlan, error) {
	return parser.WatchPlan{}, nil
}

func (p *cursorRetryProvider) SourcesForChangedPath(
	_ context.Context,
	req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	if filepath.Clean(req.Path) != filepath.Clean(p.factory.source.DisplayPath) {
		return nil, nil
	}
	return []parser.SourceRef{p.factory.source}, nil
}

func (p *cursorRetryProvider) FindSource(
	_ context.Context,
	_ parser.FindSourceRequest,
) (parser.SourceRef, bool, error) {
	return p.factory.source, true, nil
}

func (p *cursorRetryProvider) SourceForReconciliation(
	_ context.Context,
	path string,
	_ string,
) (parser.SourceRef, bool, error) {
	if p.factory.rehydrationErr != nil {
		return parser.SourceRef{}, false, p.factory.rehydrationErr
	}
	sources := p.factory.sources
	if len(sources) == 0 {
		sources = []parser.SourceRef{p.factory.source}
	}
	for _, source := range sources {
		if filepath.Clean(path) == filepath.Clean(source.DisplayPath) {
			return source, true, nil
		}
	}
	if p.factory.reconciliationErr != nil {
		return parser.SourceRef{}, false, p.factory.reconciliationErr
	}
	return parser.SourceRef{}, false, nil
}

func (p *cursorRetryProvider) DependentSourceRootSessionID(
	source parser.SourceRef,
) (string, bool) {
	if p.factory.dependentRootID == "" ||
		source.Key != p.factory.source.Key {
		return "", false
	}
	return p.factory.dependentRootID, true
}

func (p *cursorRetryProvider) Fingerprint(
	_ context.Context,
	source parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{
		Key: source.Key, Size: 2, MTimeNS: 1234, Hash: "cursor-hash",
	}, nil
}

func (p *cursorRetryProvider) Parse(
	_ context.Context,
	req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	p.factory.mu.Lock()
	p.factory.parseCalls++
	p.factory.mu.Unlock()
	if err := p.factory.parseErrors[req.Source.Key]; err != nil {
		return parser.ParseOutcome{}, err
	}
	started := time.Unix(1704067200, 0)
	def := p.factory.Definition()
	sessionID := def.IDPrefix + "session"
	if len(p.factory.sources) > 1 {
		sessionID = def.IDPrefix + filepath.Base(req.Source.DisplayPath)
	}
	return parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{{
			Result: parser.ParseResult{Session: parser.ParsedSession{
				ID: sessionID, Agent: def.Type,
				Machine: "local", Project: "project",
				StartedAt: started, EndedAt: started,
				File: parser.FileInfo{
					Path: req.Source.DisplayPath,
					Size: 2, Mtime: 1234, Hash: "cursor-hash",
				},
			}},
			DataVersion: parser.DataVersionCurrent,
		}},
		ResultSetComplete: true,
		ForceReplace:      true,
	}, nil
}

func (p *cursorRetryProvider) SourceSyncSemantics(
	parser.SourceRef,
) parser.SourceSyncSemantics {
	if !p.factory.cacheAfterWrite {
		return parser.SourceSyncSemantics{}
	}
	return parser.SourceSyncSemantics{
		BackingContainerPath:           p.factory.source.DisplayPath,
		CacheAfterWrite:                true,
		CacheKeyIncludesDataVersion:    true,
		SkipCacheFreshWithoutStoredRow: true,
	}
}

func newCursorRetryEngine(
	t *testing.T,
	database *db.DB,
	root string,
	factory *cursorRetryFactory,
) *Engine {
	t.Helper()
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			cursorRetryTestAgent: {root},
		},
		Machine:           "local",
		ProviderFactories: []parser.ProviderFactory{factory},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			cursorRetryTestAgent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	return engine
}

func TestScheduledProviderReconciliationHonorsAndAcknowledgesCursorRetry(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "cursor.db")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	factory := &cursorRetryFactory{
		root: root,
		source: parser.SourceRef{
			Provider: cursorRetryTestAgent,
			Key:      path, DisplayPath: path, FingerprintKey: path,
		},
	}
	engine := newCursorRetryEngine(t, database, root, factory)
	firstWrite := true
	engine.writeBatchOverride = func(
		batch []pendingWrite, mode syncWriteMode, force bool,
	) (int, int, int, int) {
		if firstWrite {
			firstWrite = false
			return 0, 0, len(batch), 0
		}
		return engine.writeBatch(batch, mode, force)
	}

	require.Error(t, engine.ReconcileProviderRoots(
		t.Context(), cursorRetryTestAgent, []string{root},
	))
	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), cursorRetryTestAgent, []string{root},
	))
	stored, err := database.GetSession(
		t.Context(), "cursor-retry:session",
	)
	require.NoError(t, err)
	require.NotNil(t, stored,
		"the forced-full scheduled retry must recover the cursor-skipped source")
	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), cursorRetryTestAgent, []string{root},
	))

	configs, parseCalls := factory.snapshot()
	assert.Equal(t, []bool{false, true, false}, configs)
	assert.Equal(t, 2, parseCalls,
		"an acknowledged retry must return the next scheduled pass to bounded discovery")
	pending, _ := engine.fullDiscoveryRetry(cursorRetryTestAgent)
	assert.False(t, pending)
}

func TestDependentSourceExpansionFailureArmsCursorRecovery(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	rootPath := filepath.Join(root, "root.db")
	childPath := filepath.Join(root, "root.db#child")
	require.NoError(t, os.WriteFile(rootPath, []byte("{}"), 0o600))
	parentID := "cursor-retry:root"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: parentID, Agent: string(cursorRetryTestAgent), Machine: "local",
		FilePath: &rootPath,
	}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "cursor-retry:child", Agent: string(cursorRetryTestAgent),
		Machine: "local", ParentSessionID: &parentID, FilePath: &childPath,
	}))
	injected := errors.New("injected dependent-source reconstruction failure")
	factory := &cursorRetryFactory{
		root: root,
		source: parser.SourceRef{
			Provider: cursorRetryTestAgent,
			Key:      rootPath, DisplayPath: rootPath, FingerprintKey: rootPath,
		},
		dependentRootID:   parentID,
		reconciliationErr: injected,
	}
	engine := newCursorRetryEngine(t, database, root, factory)

	err := engine.SyncPathsContext(t.Context(), []string{rootPath})

	require.ErrorIs(t, err, injected)
	pending, generation := engine.fullDiscoveryRetry(cursorRetryTestAgent)
	assert.True(t, pending)
	assert.NotZero(t, generation)
}

func TestStreamedPostCursorFailuresArmFullDiscoveryRecovery(t *testing.T) {
	tests := []struct {
		name      string
		configure func(
			*testing.T, *Engine, *cursorRetryFactory,
		) context.Context
		clearFailure func(*Engine, *cursorRetryFactory)
		wantError    error
	}{
		{
			name: "spool add",
			configure: func(
				t *testing.T, engine *Engine, _ *cursorRetryFactory,
			) context.Context {
				t.Helper()
				injected := errors.New("injected reconciliation spool add failure")
				engine.reconciliationSpoolFactory = func(
					path string,
				) (reconciliationSpoolStore, error) {
					spool, err := newReconciliationSpool(path)
					if err != nil {
						return nil, err
					}
					return &failingReconciliationSpool{
						reconciliationSpoolStore: spool,
						err:                      injected,
						failWrite:                true,
					}, nil
				}
				return t.Context()
			},
			clearFailure: func(engine *Engine, _ *cursorRetryFactory) {
				engine.reconciliationSpoolFactory = func(
					path string,
				) (reconciliationSpoolStore, error) {
					return newReconciliationSpool(path)
				}
			},
			wantError: errors.New("injected reconciliation spool add failure"),
		},
		{
			name: "spool page",
			configure: func(
				t *testing.T, engine *Engine, _ *cursorRetryFactory,
			) context.Context {
				t.Helper()
				injected := errors.New("injected reconciliation spool page failure")
				engine.reconciliationSpoolFactory = func(
					path string,
				) (reconciliationSpoolStore, error) {
					spool, err := newReconciliationSpool(path)
					if err != nil {
						return nil, err
					}
					return &failingReconciliationSpool{
						reconciliationSpoolStore: spool,
						err:                      injected,
					}, nil
				}
				return t.Context()
			},
			clearFailure: func(engine *Engine, _ *cursorRetryFactory) {
				engine.reconciliationSpoolFactory = func(
					path string,
				) (reconciliationSpoolStore, error) {
					return newReconciliationSpool(path)
				}
			},
			wantError: errors.New("injected reconciliation spool page failure"),
		},
		{
			name: "rehydration",
			configure: func(
				t *testing.T, _ *Engine, factory *cursorRetryFactory,
			) context.Context {
				t.Helper()
				factory.rehydrationErr = errors.New(
					"injected reconciliation rehydration failure",
				)
				return t.Context()
			},
			clearFailure: func(_ *Engine, factory *cursorRetryFactory) {
				factory.rehydrationErr = nil
			},
			wantError: errors.New("injected reconciliation rehydration failure"),
		},
		{
			name: "cancellation",
			configure: func(
				t *testing.T, _ *Engine, factory *cursorRetryFactory,
			) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				factory.cancelAfterYield = cancel
				return ctx
			},
			clearFailure: func(_ *Engine, factory *cursorRetryFactory) {
				factory.cancelAfterYield = nil
			},
			wantError: context.Canceled,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			path := filepath.Join(root, "cursor.db")
			require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
			factory := &cursorRetryFactory{
				root: root,
				source: parser.SourceRef{
					Provider: cursorRetryTestAgent,
					Key:      path, DisplayPath: path, FingerprintKey: path,
				},
			}
			engine := newCursorRetryEngine(t, database, root, factory)
			ctx := tc.configure(t, engine, factory)

			err := engine.ReconcileProviderRoots(
				ctx, cursorRetryTestAgent, []string{root},
			)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantError.Error())
			pending, generation := engine.fullDiscoveryRetry(cursorRetryTestAgent)
			assert.True(t, pending,
				"a cursor-advanced source must not be stranded after downstream failure")
			assert.NotZero(t, generation)

			tc.clearFailure(engine, factory)
			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), cursorRetryTestAgent, []string{root},
			))
			stored, err := database.GetSession(
				t.Context(), "cursor-retry:session",
			)
			require.NoError(t, err)
			require.NotNil(t, stored,
				"the forced-full retry must recover the cursor-skipped source")
			require.NoError(t, engine.ReconcileProviderRoots(
				t.Context(), cursorRetryTestAgent, []string{root},
			))
			configs, parseCalls := factory.snapshot()
			assert.Equal(t, []bool{false, true, false}, configs)
			assert.Equal(t, 1, parseCalls)
			pending, _ = engine.fullDiscoveryRetry(cursorRetryTestAgent)
			assert.False(t, pending,
				"a successful forced-full pass must acknowledge the retry")
		})
	}
}

func TestStreamedProviderFailureDoesNotArmUnrelatedCursorProvider(
	t *testing.T,
) {
	const (
		failingAgent parser.AgentType = "cursor-retry-failing"
		healthyAgent parser.AgentType = "cursor-retry-healthy"
	)
	database := openTestDB(t)
	root := t.TempDir()
	failingPath := filepath.Join(root, "failing.db")
	healthyPath := filepath.Join(root, "healthy.db")
	require.NoError(t, os.WriteFile(failingPath, []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(healthyPath, []byte("{}"), 0o600))
	failingFactory := &cursorRetryFactory{
		agent: failingAgent,
		root:  root,
		source: parser.SourceRef{
			Provider: failingAgent,
			Key:      failingPath, DisplayPath: failingPath,
			FingerprintKey: failingPath,
		},
		discoveryErr: errors.New("injected provider discovery failure"),
	}
	healthyFactory := &cursorRetryFactory{
		agent: healthyAgent,
		root:  root,
		source: parser.SourceRef{
			Provider: healthyAgent,
			Key:      healthyPath, DisplayPath: healthyPath,
			FingerprintKey: healthyPath,
		},
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			failingAgent: {root},
			healthyAgent: {root},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{
			failingFactory,
			healthyFactory,
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			failingAgent: parser.ProviderMigrationProviderAuthoritative,
			healthyAgent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	require.Error(t, engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	))
	failingPending, failingGeneration := engine.fullDiscoveryRetry(failingAgent)
	assert.True(t, failingPending)
	assert.NotZero(t, failingGeneration)
	healthyPending, healthyGeneration := engine.fullDiscoveryRetry(healthyAgent)
	assert.False(t, healthyPending,
		"one provider's discovery failure must not force an unrelated archive-wide pass")
	assert.Zero(t, healthyGeneration)

	require.NoError(t, engine.ReconcileProviderRoots(
		t.Context(), healthyAgent, []string{root},
	))
	healthyConfigs, _ := healthyFactory.snapshot()
	assert.Equal(t, []bool{true, false}, healthyConfigs,
		"the unrelated provider must remain on bounded discovery")
}

func TestStreamedLaterPageFailureArmsOnlyAffectedProvider(t *testing.T) {
	const (
		committedAgent parser.AgentType = "cursor-page-a-committed"
		failingAgent   parser.AgentType = "cursor-page-z-failing"
	)
	database := openTestDB(t)
	root := t.TempDir()
	committedSources := make(
		[]parser.SourceRef, 0, reconciliationPageSize,
	)
	for i := range reconciliationPageSize {
		path := filepath.Join(root, fmt.Sprintf("committed-%03d.db", i))
		require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
		committedSources = append(committedSources, parser.SourceRef{
			Provider: committedAgent,
			Key:      path, DisplayPath: path, FingerprintKey: path,
		})
	}
	// Discovery may yield the same provider identity more than once. The spool
	// keeps only one row, so recovery accounting must not retain a phantom
	// candidate after the unique first page commits.
	committedSources = append(committedSources, committedSources[0])
	failingPath := filepath.Join(root, "failing.db")
	require.NoError(t, os.WriteFile(failingPath, []byte("{}"), 0o600))
	committedFactory := &cursorRetryFactory{
		agent: committedAgent, root: root, sources: committedSources,
	}
	failingFactory := &cursorRetryFactory{
		agent: failingAgent,
		root:  root,
		source: parser.SourceRef{
			Provider: failingAgent,
			Key:      failingPath, DisplayPath: failingPath,
			FingerprintKey: failingPath,
		},
		parseErrors: map[string]error{
			failingPath: errors.New("injected later-page parse failure"),
		},
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			committedAgent: {root},
			failingAgent:   {root},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{
			committedFactory,
			failingFactory,
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			committedAgent: parser.ProviderMigrationProviderAuthoritative,
			failingAgent:   parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)

	err := engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "failed processing page")
	committedPending, committedGeneration :=
		engine.fullDiscoveryRetry(committedAgent)
	assert.False(t, committedPending,
		"a provider whose earlier page committed and baselined must stay bounded")
	assert.Zero(t, committedGeneration)
	failingPending, failingGeneration := engine.fullDiscoveryRetry(failingAgent)
	assert.True(t, failingPending,
		"the provider whose cursor source failed must be rediscovered fully")
	assert.NotZero(t, failingGeneration)
	stored, getErr := database.GetSession(
		t.Context(),
		string(committedAgent)+":committed-000.db",
	)
	require.NoError(t, getErr)
	assert.NotNil(t, stored,
		"the regression requires the earlier provider page to be durable")
}

func TestPersistentGlobalLinkFailureKeepsCursorProvidersBounded(t *testing.T) {
	const (
		firstAgent  parser.AgentType = "cursor-link-a"
		secondAgent parser.AgentType = "cursor-link-b"
	)
	database := openTestDB(t)
	root := t.TempDir()
	newFactory := func(
		t *testing.T, agent parser.AgentType, filename string,
	) *cursorRetryFactory {
		t.Helper()
		path := filepath.Join(root, filename)
		require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
		return &cursorRetryFactory{
			agent: agent,
			root:  root,
			source: parser.SourceRef{
				Provider: agent,
				Key:      path, DisplayPath: path, FingerprintKey: path,
			},
		}
	}
	firstFactory := newFactory(t, firstAgent, "first.db")
	secondFactory := newFactory(t, secondAgent, "second.db")
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			firstAgent:  {root},
			secondAgent: {root},
		},
		Machine: "local",
		ProviderFactories: []parser.ProviderFactory{
			firstFactory,
			secondFactory,
		},
		ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			firstAgent:  parser.ProviderMigrationProviderAuthoritative,
			secondAgent: parser.ProviderMigrationProviderAuthoritative,
		},
	})
	t.Cleanup(engine.Close)
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "link-parent", Agent: string(firstAgent), Project: "project",
		Machine: "local",
	}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "link-child", Agent: string(firstAgent), Project: "project",
		Machine: "local",
	}))
	require.NoError(t, database.InsertMessages([]db.Message{{
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
	_, err = raw.Exec(`
		CREATE TRIGGER fail_persistent_subagent_link
		BEFORE UPDATE OF relationship_type ON sessions
		WHEN NEW.relationship_type = 'subagent'
		BEGIN
			SELECT RAISE(FAIL, 'injected persistent linking failure');
		END;
	`)
	require.NoError(t, err)

	err = engine.ReconcileWatchRoots(
		t.Context(), []string{root}, false,
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, "link subagent sessions")
	for _, agent := range []parser.AgentType{firstAgent, secondAgent} {
		pending, generation := engine.fullDiscoveryRetry(agent)
		assert.Falsef(t, pending,
			"a global link retry must not force %s into full discovery", agent)
		assert.Zerof(t, generation,
			"a global link retry must not allocate %s a cursor retry generation", agent)
	}

	require.Error(t, engine.ReconcileProviderRoots(
		t.Context(), firstAgent, []string{root},
	))
	require.Error(t, engine.ReconcileProviderRoots(
		t.Context(), secondAgent, []string{root},
	))
	firstConfigs, _ := firstFactory.snapshot()
	secondConfigs, _ := secondFactory.snapshot()
	assert.Equal(t, []bool{true, false}, firstConfigs,
		"the next provider-scoped link retry must stay bounded")
	assert.Equal(t, []bool{true, false}, secondConfigs,
		"persistent global work must not amplify either provider's discovery")
}

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
					DiscoverSources:            parser.CapabilitySupported,
					StreamingDiscovery:         parser.CapabilitySupported,
					IncrementalDiscoveryCursor: parser.CapabilitySupported,
					MultiSessionSource:         parser.CapabilitySupported,
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
	pending, generation := engine.fullDiscoveryRetry(semanticTestAgent)
	assert.True(t, pending,
		"a failed baseline must recover the provider's cursor source")
	assert.NotZero(t, generation)
}

func TestReconciliationBaselineFailureCannotPromoteContainerCache(
	t *testing.T,
) {
	database := openTestDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "container.db")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	factory := &cursorRetryFactory{
		root: root,
		source: parser.SourceRef{
			Provider: cursorRetryTestAgent,
			Key:      path, DisplayPath: path, FingerprintKey: path,
		},
		cacheAfterWrite: true,
	}
	engine := newCursorRetryEngine(t, database, root, factory)
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
	pending, _ := engine.fullDiscoveryRetry(cursorRetryTestAgent)
	assert.True(t, pending,
		"a failed baseline must force complete rediscovery of cursor sources")
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
