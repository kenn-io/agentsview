package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

type issue1476WatchProvider struct {
	parser.ProviderBase
	root              string
	source            parser.SourceRef
	sourcesByPath     map[string][]parser.SourceRef
	outcome           parser.ParseOutcome
	outcomes          map[string]parser.ParseOutcome
	classificationErr error
	discoverSource    *parser.SourceRef
	discoverErr       error
	discoverCalls     *atomic.Int32
}

func TestSyncStatsProcessingCompleteTruthTable(t *testing.T) {
	tests := []struct {
		name          string
		stats         SyncStats
		wantOK        bool
		wantDiscovery bool
	}{
		{name: "clean", wantOK: true, wantDiscovery: true},
		{name: "deferred", stats: SyncStats{Deferred: 1}, wantDiscovery: true},
		{name: "hard failure", stats: SyncStats{Failed: 1}, wantDiscovery: true},
		{name: "provider failure", stats: SyncStats{providerFailures: 1}},
		{name: "aborted", stats: SyncStats{Aborted: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantOK, tt.stats.ProcessingComplete())
			assert.Equal(t, tt.wantDiscovery, tt.stats.AuthoritativeDiscoveryComplete())
		})
	}
}

func (p *issue1476WatchProvider) WatchPlan(context.Context) (parser.WatchPlan, error) {
	return parser.WatchPlan{Roots: []parser.WatchRoot{{Path: p.root}}}, nil
}

func (p *issue1476WatchProvider) SourcesForChangedPath(
	_ context.Context, req parser.ChangedPathRequest,
) ([]parser.SourceRef, error) {
	if p.classificationErr != nil {
		return nil, p.classificationErr
	}
	if sources, ok := p.sourcesByPath[req.Path]; ok {
		return append([]parser.SourceRef(nil), sources...), nil
	}
	return []parser.SourceRef{p.source}, nil
}

func (p *issue1476WatchProvider) DiscoverEach(
	_ context.Context, yield func(parser.SourceRef) error,
) error {
	if p.discoverCalls != nil {
		p.discoverCalls.Add(1)
	}
	if p.discoverSource != nil {
		if err := yield(*p.discoverSource); err != nil {
			return err
		}
	}
	return p.discoverErr
}

func (p *issue1476WatchProvider) Fingerprint(
	context.Context, parser.SourceRef,
) (parser.SourceFingerprint, error) {
	return parser.SourceFingerprint{}, nil
}

func (p *issue1476WatchProvider) Parse(
	_ context.Context, req parser.ParseRequest,
) (parser.ParseOutcome, error) {
	if outcome, ok := p.outcomes[req.Source.DisplayPath]; ok {
		return outcome, nil
	}
	return p.outcome, nil
}

type issue1476WatchFactory struct{ provider *issue1476WatchProvider }

func (f issue1476WatchFactory) Definition() parser.AgentDef       { return f.provider.Definition() }
func (f issue1476WatchFactory) Capabilities() parser.Capabilities { return f.provider.Capabilities() }
func (f issue1476WatchFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	clone := *f.provider
	clone.Config = cfg.Clone()
	return &clone
}

func newIssue1476WatchEngine(
	t *testing.T, root string, providers ...*issue1476WatchProvider,
) *Engine {
	t.Helper()
	factories := make([]parser.ProviderFactory, 0, len(providers))
	agentDirs := make(map[parser.AgentType][]string, len(providers))
	modes := make(map[parser.AgentType]parser.ProviderMigrationMode, len(providers))
	for _, provider := range providers {
		factories = append(factories, issue1476WatchFactory{provider: provider})
		agent := provider.Def.Type
		agentDirs[agent] = []string{root}
		modes[agent] = parser.ProviderMigrationProviderAuthoritative
	}
	engine := NewEngine(openTestDB(t), EngineConfig{
		AgentDirs: agentDirs, Machine: "local", ProviderFactories: factories,
		ProviderMigrationModes: modes,
	})
	t.Cleanup(engine.Close)
	return engine
}

func issue1476DeferredOutcome(path string, agent parser.AgentType) parser.ParseOutcome {
	started := time.Unix(1704067200, 0)
	return parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{{
			Result: parser.ParseResult{Session: parser.ParsedSession{
				ID: "deferred", Agent: agent, Project: "project", Machine: "local",
				StartedAt: started, EndedAt: started, File: parser.FileInfo{Path: path},
			}},
			DataVersion: parser.DataVersionNeedsRetry,
		}},
		ResultSetComplete: true,
	}
}

func issue1476CurrentOutcome(path string, agent parser.AgentType) parser.ParseOutcome {
	started := time.Unix(1704067200, 0)
	return parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{{
			Result: parser.ParseResult{Session: parser.ParsedSession{
				ID: "root-session", Agent: agent, Project: "project", Machine: "local",
				StartedAt: started, EndedAt: started, File: parser.FileInfo{Path: path},
			}},
			DataVersion: parser.DataVersionCurrent,
		}},
		ResultSetComplete: true,
	}
}

func issue1476WatchCapabilities() parser.Capabilities {
	return parser.Capabilities{Source: parser.SourceCapabilities{
		DiscoverSources:    parser.CapabilitySupported,
		StreamingDiscovery: parser.CapabilitySupported,
		WatchSources:       parser.CapabilitySupported,
		FindSource:         parser.CapabilitySupported,
	}}
}

func TestSyncWatchBatchThenRunDeferredPathExecutesPlannedRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "deferred.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	rootPath := filepath.Join(root, "root-session.jsonl")
	require.NoError(t, os.WriteFile(rootPath, []byte("{}\n"), 0o600))
	var discoverCalls atomic.Int32
	rootSource := parser.SourceRef{
		Provider: parser.AgentCodex, Key: rootPath, DisplayPath: rootPath, FingerprintKey: rootPath,
	}
	provider := &issue1476WatchProvider{
		Def:  parser.AgentDef{Type: parser.AgentCodex, FileBased: true},
		Caps: issue1476WatchCapabilities(),
		root: root, source: parser.SourceRef{
			Provider: parser.AgentCodex, Key: path, DisplayPath: path, FingerprintKey: path,
		},
		outcome:        issue1476DeferredOutcome(path, parser.AgentCodex),
		outcomes:       map[string]parser.ParseOutcome{rootPath: issue1476CurrentOutcome(rootPath, parser.AgentCodex)},
		sourcesByPath:  map[string][]parser.SourceRef{rootPath: {rootSource}},
		discoverSource: &rootSource,
		discoverCalls:  &discoverCalls,
	}
	engine := newIssue1476WatchEngine(t, root, provider)

	workCalled := false
	_, err := engine.SyncWatchBatchThenRun(t.Context(), WatchBatch{
		Paths: []string{path}, ReconcileRoots: []string{root},
	}, nil, func() error {
		workCalled = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, workCalled,
		"deferred processing must not run post-sync acknowledgement work")
	assert.Greater(t, discoverCalls.Load(), int32(0),
		"producer-marked defer-only paths must allow one planned root phase")
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.ErrorAs(t, err, &retry)
	assert.Equal(t, []string{path}, retry.WatchRetryBatch().Paths)
	assert.Empty(t, retry.WatchRetryBatch().ReconcileRoots,
		"a successful root must not be retried solely because the path is deferred")
	stored, dbErr := engine.db.GetSession(t.Context(), "root-session")
	require.NoError(t, dbErr)
	require.NotNil(t, stored, "root reconciliation must discover, parse, and write the root session")
}

func TestSyncWatchBatchThenRunComposesDeferredPathAndRootFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "deferred.jsonl")
	rootPath := filepath.Join(root, "root-session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(rootPath, []byte("{}\n"), 0o600))
	rootSource := parser.SourceRef{
		Provider: parser.AgentCodex, Key: rootPath, DisplayPath: rootPath, FingerprintKey: rootPath,
	}
	rootCause := errors.New("root failure")
	provider := &issue1476WatchProvider{
		Def:  parser.AgentDef{Type: parser.AgentCodex, FileBased: true},
		Caps: issue1476WatchCapabilities(),
		root: root, source: parser.SourceRef{
			Provider: parser.AgentCodex, Key: path, DisplayPath: path, FingerprintKey: path,
		},
		outcome:        issue1476DeferredOutcome(path, parser.AgentCodex),
		sourcesByPath:  map[string][]parser.SourceRef{rootPath: {rootSource}},
		outcomes:       map[string]parser.ParseOutcome{rootPath: issue1476CurrentOutcome(rootPath, parser.AgentCodex)},
		discoverSource: &rootSource, discoverErr: rootCause,
	}
	engine := newIssue1476WatchEngine(t, root, provider)

	_, err := engine.SyncWatchBatchThenRun(t.Context(), WatchBatch{
		Paths: []string{path}, ReconcileRoots: []string{root}, LostEvents: true,
	}, nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, rootCause)
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.ErrorAs(t, err, &retry)
	assert.Equal(t, WatchBatch{
		Paths: []string{path}, ReconcileRoots: []string{root}, LostEvents: true,
	}, retry.WatchRetryBatch())
}

func TestSyncWatchBatchThenRunMixedDeferredErrorsSuppressRoots(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "deferred.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	const deferredAgent parser.AgentType = parser.AgentCodex
	const classificationAgent parser.AgentType = "issue1476-classification-mixed"
	var discoverCalls atomic.Int32
	pathProvider := &issue1476WatchProvider{
		Def: parser.AgentDef{Type: deferredAgent, FileBased: true}, Caps: issue1476WatchCapabilities(),
		root: root, source: parser.SourceRef{
			Provider: deferredAgent, Key: path, DisplayPath: path, FingerprintKey: path,
		},
		outcome: issue1476DeferredOutcome(path, deferredAgent), discoverCalls: &discoverCalls,
	}
	classificationCause := errors.New("classification cause")
	classificationProvider := &issue1476WatchProvider{
		Def: parser.AgentDef{Type: classificationAgent, FileBased: true}, Caps: issue1476WatchCapabilities(),
		root: root, classificationErr: classificationCause,
	}
	engine := newIssue1476WatchEngine(t, root, pathProvider, classificationProvider)

	_, err := engine.SyncWatchBatchThenRun(t.Context(), WatchBatch{
		Paths: []string{path}, ReconcileRoots: []string{root},
	}, nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, classificationCause)
	var deferOnly interface{ ReconciliationRetryDeferOnly() bool }
	require.ErrorAs(t, err, &deferOnly)
	assert.False(t, deferOnly.ReconciliationRetryDeferOnly())
	assert.Zero(t, discoverCalls.Load(), "mixed path errors must suppress roots")
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.ErrorAs(t, err, &retry)
	assert.Equal(t, WatchBatch{Paths: []string{path}, ReconcileRoots: []string{root}}, retry.WatchRetryBatch())
}

func TestSyncWatchBatchThenRunDeferredCancellationSuppressesRoots(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "deferred.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	var discoverCalls atomic.Int32
	provider := &issue1476WatchProvider{
		Def: parser.AgentDef{Type: parser.AgentCodex, FileBased: true}, Caps: issue1476WatchCapabilities(),
		root: root, source: parser.SourceRef{
			Provider: parser.AgentCodex, Key: path, DisplayPath: path, FingerprintKey: path,
		},
		outcome:       issue1476DeferredOutcome(path, parser.AgentCodex),
		discoverCalls: &discoverCalls,
	}
	cancellationProvider := &issue1476WatchProvider{
		Def: parser.AgentDef{Type: "issue1476-cancellation", FileBased: true}, Caps: issue1476WatchCapabilities(),
		root: root, classificationErr: context.Canceled,
	}
	engine := newIssue1476WatchEngine(t, root, provider, cancellationProvider)

	_, err := engine.SyncWatchBatchThenRun(t.Context(), WatchBatch{
		Paths: []string{path}, ReconcileRoots: []string{root},
	}, nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	var deferOnly interface{ ReconciliationRetryDeferOnly() bool }
	require.ErrorAs(t, err, &deferOnly)
	assert.False(t, deferOnly.ReconciliationRetryDeferOnly())
	assert.Zero(t, discoverCalls.Load(), "deferred cancellation must suppress roots")
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.ErrorAs(t, err, &retry)
	assert.Equal(t, WatchBatch{Paths: []string{path}, ReconcileRoots: []string{root}}, retry.WatchRetryBatch())
}

func TestSyncWatchBatchThenRunDeferredHardFailureSuppressesRoots(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "deferred.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
	var discoverCalls atomic.Int32
	outcome := issue1476DeferredOutcome(path, parser.AgentCodex)
	outcome.SourceErrors = []parser.SourceError{{
		SourceKey: path, SessionID: "hard-source", Err: errors.New("hard source failure"),
	}}
	provider := &issue1476WatchProvider{
		Def:  parser.AgentDef{Type: parser.AgentCodex, FileBased: true},
		Caps: issue1476WatchCapabilities(),
		root: root, source: parser.SourceRef{
			Provider: parser.AgentCodex, Key: path, DisplayPath: path, FingerprintKey: path,
		},
		outcome: outcome, discoverCalls: &discoverCalls,
	}
	engine := newIssue1476WatchEngine(t, root, provider)

	_, err := engine.SyncWatchBatchThenRun(t.Context(), WatchBatch{
		Paths: []string{path}, ReconcileRoots: []string{root},
	}, nil, nil)
	require.Error(t, err)
	var deferOnly interface{ ReconciliationRetryDeferOnly() bool }
	require.ErrorAs(t, err, &deferOnly)
	assert.False(t, deferOnly.ReconciliationRetryDeferOnly())
	assert.Zero(t, discoverCalls.Load(), "deferred plus hard failure must suppress roots")
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.ErrorAs(t, err, &retry)
	assert.Equal(t, WatchBatch{Paths: []string{path}, ReconcileRoots: []string{root}}, retry.WatchRetryBatch())
}

func TestIssue1476SourceProofWithheldCoversDeferredAndHardFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  processResult
		hard bool
	}{
		{name: "hard failure", hard: true},
		{name: "provider failure", job: processResult{providerFailureCount: 1}},
		{name: "deferred count", job: processResult{deferredCount: 1}},
		{name: "retry session", job: processResult{
			retrySessionIDs: map[string]bool{"forked-child": true},
		}},
		{name: "legacy retry", job: processResult{needsRetry: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, tc.job.sourceProofWithheld(tc.hard))
		})
	}
	assert.False(t, (processResult{}).sourceProofWithheld(false))
	t.Log("proof gate: hard failure, deferred count, retry session IDs, and legacy retry all withhold")
}

func TestIssue1476DeferredRetryPathsRemainBounded(t *testing.T) {
	stats := SyncStats{}
	for i := range reconciliationRetryPathLimit + 1 {
		stats.recordDeferred(strings.Repeat("x", 16) + strconv.Itoa(i))
	}
	assert.True(t, stats.deferredRetryOverflow)
	assert.Empty(t, stats.deferredRetryPaths)
	byteBound := SyncStats{}
	byteBound.recordDeferred(strings.Repeat("y", reconciliationRetryPathByteLimit+1))
	assert.True(t, byteBound.deferredRetryOverflow)
	assert.Empty(t, byteBound.deferredRetryPaths)
	t.Logf("bounded retry scope: %d paths exceed the exact-path bound and fall back to roots", stats.Deferred)
}

func TestIssue1476OverflowRetryBatchReachesWatcherBackoff(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sourceCount int
		pathLength  int
	}{
		// Count and byte overflow share deferredRetryOverflow; the direct test above
		// covers both bounds. Keep the watcher integration on the cheap byte case.
		// One oversized path crosses the byte cap without making callback timing
		// depend on hundreds of database writes under race instrumentation.
		{name: "bytes", sourceCount: 1, pathLength: reconciliationRetryPathByteLimit + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			const agent parser.AgentType = parser.AgentCodex
			sources := make([]parser.SourceRef, tc.sourceCount)
			outcomes := make(map[string]parser.ParseOutcome, tc.sourceCount)
			started := time.Unix(1704067200, 0)
			for i := range sources {
				path := filepath.Join(root, fmt.Sprintf("session-%03d-%s.jsonl", i, strings.Repeat("x", tc.pathLength)))
				sources[i] = parser.SourceRef{
					Provider: agent, Key: path, DisplayPath: path, FingerprintKey: path,
				}
				outcomes[path] = parser.ParseOutcome{
					Results: []parser.ParseResultOutcome{{
						Result: parser.ParseResult{Session: parser.ParsedSession{
							ID: fmt.Sprintf("session-%03d", i), Agent: agent,
							Project: "project", Machine: "local", StartedAt: started,
							EndedAt: started, File: parser.FileInfo{Path: path},
						}},
						DataVersion: parser.DataVersionNeedsRetry,
					}}, ResultSetComplete: true,
				}
			}
			provider := &manyStreamingProvider{
				Def: parser.AgentDef{Type: agent, FileBased: true},
				Caps: parser.Capabilities{Source: parser.SourceCapabilities{
					StreamingDiscovery: parser.CapabilitySupported,
					WatchSources:       parser.CapabilitySupported,
				}},
				sources: sources, parseOutcomes: outcomes,
			}
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{agent: {root}}, Machine: "local",
				ProviderFactories: []parser.ProviderFactory{manyStreamingFactory{provider}},
				ProviderMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
					agent: parser.ProviderMigrationProviderAuthoritative,
				},
			})
			t.Cleanup(engine.Close)

			backend := newFakeWatchBackend()
			calls := make(chan WatchBatch, 2)
			startedAt := make(chan time.Time, 2)
			statsCh := make(chan SyncStats, 1)
			const retryFloor = 20 * time.Millisecond
			watcher, err := newWatcherWithBackend(
				0, retryFloor,
				func(ctx context.Context, batch WatchBatch) error {
					calls <- batch
					startedAt <- time.Now()
					stats, err := engine.SyncWatchBatchThenRun(ctx, batch, nil, nil)
					if stats.deferredRetryOverflow {
						statsCh <- stats
					}
					return err
				},
				backend, defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
			)
			require.NoError(t, err)
			watcher.Start()
			t.Cleanup(watcher.Stop)

			backend.sendBackendEvent(t, backendEvent{
				Path: root, Root: root, Op: backendOpReconcileRootChange,
			})
			first := receiveWatchBatch(t, calls)
			second := receiveWatchBatch(t, calls)
			stats := <-statsCh
			firstStarted := <-startedAt
			secondStarted := <-startedAt

			assert.Equal(t, []string{root}, first.ReconcileRoots)
			assert.False(t, first.FullSync)
			assert.Equal(t, []string{root}, second.ReconcileRoots,
				"overflow must propagate the affected root into the watcher retry")
			assert.False(t, second.FullSync)
			assert.True(t, stats.deferredRetryOverflow)
			assert.GreaterOrEqual(t, secondStarted.Sub(firstStarted), retryFloor,
				"affected-root retry must use watcher backoff")
		})
	}
}

func TestIssue1476ChangedPathRetryBatchRetainsDeferredPath(t *testing.T) {
	const agent parser.AgentType = parser.AgentCodex
	_, engine, _, _, path := newChangedPathOutcomeEngine(
		t, agent, func(path string) parser.ParseOutcome {
			started := time.Unix(1704067200, 0)
			return parser.ParseOutcome{
				Results: []parser.ParseResultOutcome{{
					Result: parser.ParseResult{Session: parser.ParsedSession{
						ID: "forked-child", Agent: agent, Project: "project",
						Machine: "local", StartedAt: started, EndedAt: started,
						File: parser.FileInfo{Path: path},
					}},
					DataVersion: parser.DataVersionNeedsRetry,
				}},
				ResultSetComplete: true,
			}
		},
	)
	t.Cleanup(engine.Close)

	_, err := engine.SyncWatchBatchThenRun(
		t.Context(), WatchBatch{Paths: []string{path}}, nil, nil,
	)
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.ErrorAs(t, err, &retry)
	var deferOnly interface{ ReconciliationRetryDeferOnly() bool }
	require.ErrorAs(t, err, &deferOnly)
	assert.True(t, deferOnly.ReconciliationRetryDeferOnly())
	assert.Equal(t, []string{path}, retry.WatchRetryBatch().Paths)
	assert.Empty(t, retry.WatchRetryBatch().ReconcileRoots)
	assert.Equal(t, 1, engine.LastSyncStats().Deferred)
}

func TestIssue1476WatchBatchKeepsExactDeferredPath(t *testing.T) {
	deferredPath := `C:\sessions\forked-child.jsonl`
	cause := &incompleteReconciliationError{
		failures: 1, deferred: 1,
		roots: []string{`C:\sessions\hard-failure`}, paths: []string{deferredPath},
	}
	err := watchBatchReconciliationError(
		cause, []string{`C:\sessions\changed.jsonl`}, []string{`C:\sessions`}, false, false,
	)
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.True(t, errors.As(err, &retry))
	assert.Equal(t, []string{deferredPath}, retry.WatchRetryBatch().Paths)
	assert.Equal(t, []string{`C:\sessions\hard-failure`}, retry.WatchRetryBatch().ReconcileRoots)
	t.Logf("exact retry scope: retained path %q", deferredPath)
}

func TestIssue1476FullOriginKeepsTypedRetryScope(t *testing.T) {
	deferredPath := `C:\\sessions\\forked-child.jsonl`
	cause := &incompleteReconciliationError{
		failures: 1, deferred: 1,
		paths: []string{deferredPath},
	}
	err := watchBatchReconciliationError(
		cause, []string{`C:\\sessions\\changed.jsonl`}, nil, true, true,
	)
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.ErrorAs(t, err, &retry)
	assert.Equal(t, WatchBatch{
		Paths: []string{deferredPath}, LostEvents: true,
	}, retry.WatchRetryBatch())
}
