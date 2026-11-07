package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSessionPushContributorMergePreservesRebuildDecision(t *testing.T) {
	for name, merge := range map[string]func(*SyncStats, SyncStats){"contributors": mergeSyncStats, "reconciliation": mergeReconciliationSyncStats} {
		t.Run(name, func(t *testing.T) {
			for _, mixed := range []bool{false, true} {
				var aggregate SyncStats
				if mixed {
					aggregate = SyncStats{TotalSessions: 1, Skipped: 1, filesOK: 1, nonContainerDiscovered: 1, deferredPreservedIDs: []string{"deferred"}}
				}
				merge(&aggregate, SyncStats{TotalSessions: 1, Skipped: 1, filesOK: 1, nonContainerDiscovered: 1, sessionPushReservedFiles: 1})
				assert.Empty(t, resyncSwapAbortReason(aggregate, 1, 0), "known reserved/deferred outcomes must survive aggregation")
				failed := aggregate
				failed.Failed++
				assert.NotEmpty(t, resyncSwapAbortReason(failed, 1, 0), "real failures still abort")
				unexplained := aggregate
				unexplained.TotalSessions++
				unexplained.filesOK++
				unexplained.Skipped++
				assert.NotEmpty(t, resyncSwapAbortReason(unexplained, 1, 0), "unexplained empty outcomes still abort")
			}
		})
	}
}
