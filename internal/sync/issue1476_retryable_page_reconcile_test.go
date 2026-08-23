package sync

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	t.Logf("bounded retry scope: %d paths exceed the exact-path bound and fall back to roots", stats.deferredCount)
}

func TestIssue1476WatchBatchKeepsExactDeferredPath(t *testing.T) {
	deferredPath := `C:\sessions\forked-child.jsonl`
	cause := &incompleteReconciliationError{
		deferred: 1, paths: []string{deferredPath},
	}
	err := watchBatchReconciliationError(cause, []string{`C:\sessions`}, false, false)
	var retry interface{ WatchRetryBatch() WatchBatch }
	require.True(t, errors.As(err, &retry))
	assert.Equal(t, []string{deferredPath}, retry.WatchRetryBatch().Paths)
	assert.Empty(t, retry.WatchRetryBatch().ReconcileRoots)
	t.Logf("exact retry scope: retained path %q", deferredPath)
}
