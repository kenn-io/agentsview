package sync

import (
	"errors"
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
	for _, name := range []string{"count", "bytes"} {
		t.Run(name, func(t *testing.T) {
			stats := SyncStats{}
			if name == "count" {
				for i := range reconciliationRetryPathLimit + 1 {
					stats.recordDeferred(strconv.Itoa(i))
				}
			} else {
				stats.recordDeferred(strings.Repeat("x", reconciliationRetryPathByteLimit+1))
			}
			retryErr := watchBatchReconciliationError(
				&incompleteReconciliationError{
					deferred: stats.deferredCount, overflow: stats.deferredRetryOverflow,
				}, nil, nil, false, false,
			)
			retry, ok := callbackRetryBatch(retryErr)
			require.True(t, ok)
			assert.True(t, retry.FullSync,
				"overflow without an affected root must use full watcher recovery")
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
