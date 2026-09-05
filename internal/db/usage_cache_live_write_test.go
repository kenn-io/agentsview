//go:build fts5

package db

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedLiveWriteSession inserts one session whose message carries a unique
// Claude identity, so sessions in these tests never share a dedup group.
func seedLiveWriteSession(
	t *testing.T, database *DB, id, project, timestamp string,
	ordinal, input int,
) {
	t.Helper()
	started := timestamp
	insertSession(t, database, id, project, func(session *Session) {
		session.StartedAt = &started
		session.Agent = "claude"
	})
	appendLiveWriteMessage(t, database, id, timestamp, ordinal, input)
}

func appendLiveWriteMessage(
	t *testing.T, database *DB, id, timestamp string, ordinal, input int,
) {
	t.Helper()
	suffix := id + "-" + strconv.Itoa(ordinal)
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: id, Ordinal: ordinal, Role: "assistant",
		Timestamp: timestamp, Model: "model-a",
		TokenUsage: json.RawMessage(
			`{"input_tokens":` + strconv.Itoa(input) + `}`),
		ClaudeMessageID: "message-" + suffix,
		ClaudeRequestID: "request-" + suffix,
	}}))
}

func liveWriteDailyInput(t *testing.T, database *DB, project string) int {
	t.Helper()
	daily, err := database.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
		Project: project, SkipSessionCounts: true,
	})
	require.NoError(t, err)
	return daily.Totals.InputTokens
}

// A rollup build reads only committed per-session facts, so an archive write
// landing while it runs can never abort it. The rollup coordinator's
// beforeEnsure seam runs on the detached build goroutine, after this request's
// facts are already filled, which is exactly the window that used to fail.
func TestUsageSummarySurvivesArchiveWriteDuringRollupBuild(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		writtenID     string
		wantDuring    []int
		wantAfterFill int
	}{
		{
			// A session the summary does not report cannot influence it at
			// all, so the answer is unchanged in both directions.
			name: "write outside the filter", writtenID: "other-project",
			wantDuring: []int{10}, wantAfterFill: 10,
		},
		{
			// An in-scope append is either invisible to this request or
			// already part of it, but never an error.
			name: "append inside the filter", writtenID: "reported",
			wantDuring: []int{10, 17}, wantAfterFill: 17,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			database := testDB(t)
			seedLiveWriteSession(t, database, "reported", "keep",
				"2026-08-10T09:00:00Z", 0, 10)
			seedLiveWriteSession(t, database, "other-project", "drop",
				"2026-08-10T09:30:00Z", 0, 4)
			require.Equal(t, 10, liveWriteDailyInput(t, database, "keep"))

			snapshot, err := database.captureUsageQuery(
				context.Background(), UsageFilter{}, usageQueryKindToken)
			require.NoError(t, err)
			cache, err := database.usageCache.Generation(
				context.Background(), snapshot.DatabaseID)
			require.NoError(t, err)
			var written atomic.Bool
			cache.rollup.observer.beforeEnsure = func() {
				if written.Swap(true) {
					return
				}
				appendLiveWriteMessage(t, database, testCase.writtenID,
					"2026-08-10T09:45:00Z", 1, 7)
			}
			assert.Contains(t, testCase.wantDuring,
				liveWriteDailyInput(t, database, "keep"))
			assert.True(t, written.Load(), "the build seam never ran")

			cache.rollup.observer.beforeEnsure = nil
			assert.Equal(t, testCase.wantAfterFill,
				liveWriteDailyInput(t, database, "keep"))
		})
	}
}

// The coverage pass used to restart itself, and then give up, whenever the
// archive moved underneath it. Facts now come from each session's own read
// snapshot, so a pass finishes against a continuously written archive.
func TestUsageCacheBackfillCompletesUnderConcurrentArchiveWrites(t *testing.T) {
	database := testDB(t)
	for index := range 4 {
		id := "churn-" + strconv.Itoa(index)
		seedLiveWriteSession(t, database, id, "project",
			"2026-08-10T09:00:00Z", 0, 1)
	}
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	// Write into the archive from both phases of the pass: while facts are
	// being extracted, and while the rollups are being aggregated.
	var appends atomic.Int32
	churn := func() {
		ordinal := int(appends.Add(1))
		appendLiveWriteMessage(t, database, "churn-0",
			"2026-08-10T09:00:00Z", ordinal, 1)
	}
	cache.fill.observer.afterExtract = func([]usageSourceVersion) { churn() }
	cache.rollup.observer.beforeEnsure = churn

	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	written := int(appends.Load())
	require.Positive(t, written)
	assert.Equal(t, 4, usageCacheCount(t, cache, "usage_cached_sessions"))
	assert.Equal(t, 4, usageCacheCount(t, cache, "usage_rollup_installs"))

	cache.fill.observer.afterExtract = nil
	cache.rollup.observer.beforeEnsure = nil
	// Every write the pass raced is still accounted for once the next
	// request refills the session it touched.
	assert.Equal(t, 4+written, liveWriteDailyInput(t, database, "project"))
}
