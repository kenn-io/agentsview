package sync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

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
	t.Logf("bounded retry scope: %d paths exceed the exact-path bound and fall back to roots", stats.deferredCount)
}

func TestIssue1476OverflowRetryBatchReachesWatcherBackoff(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sourceCount int
		pathLength  int
	}{
		{name: "count", sourceCount: reconciliationRetryPathLimit + 1},
		{name: "bytes", sourceCount: reconciliationRetryPathLimit, pathLength: reconciliationRetryPathByteLimit/reconciliationRetryPathLimit + 1},
	} {
		tc := tc
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
				ProviderBase: parser.ProviderBase{
					Def: parser.AgentDef{Type: agent, FileBased: true},
					Caps: parser.Capabilities{Source: parser.SourceCapabilities{
						StreamingDiscovery: parser.CapabilitySupported,
						WatchSources:       parser.CapabilitySupported,
					}},
				},
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
	assert.Equal(t, []string{path}, retry.WatchRetryBatch().Paths)
	assert.Empty(t, retry.WatchRetryBatch().ReconcileRoots)
	assert.Equal(t, 1, engine.LastSyncStats().deferredCount)
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
	assert.Equal(t, []string{`C:\sessions\changed.jsonl`, deferredPath}, retry.WatchRetryBatch().Paths)
	assert.Equal(t, []string{`C:\sessions`, `C:\sessions\hard-failure`}, retry.WatchRetryBatch().ReconcileRoots)
	t.Logf("exact retry scope: retained path %q", deferredPath)
}
