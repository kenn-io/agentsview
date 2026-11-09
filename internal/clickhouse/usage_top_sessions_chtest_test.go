//go:build chtest

package clickhouse

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

func TestTopSessionsWinnerMetadata(t *testing.T) {
	t.Parallel()
	local, syncer, store := newPreparedUsageStore(t)
	var writes []db.SessionBatchWrite
	for i, name := range []string{"first fallback", "", "third fallback"} {
		id := fmt.Sprintf("metadata-%d", i)
		session := fixtureSession(id, "metadata", name, "2026-01-12T03:00:00.000Z", 1)
		if i == 2 {
			session.SessionName = new("session title")
		}
		writes = append(writes, db.SessionBatchWrite{Session: session, Messages: []db.Message{
			usagePriceMessage(id, 0, "2026-01-12T03:00:00.000Z", "round-test", fmt.Sprintf(`{"input_tokens":1000000,"cache_read_input_tokens":%d}`, (i+1)*10)),
		}, DataVersion: 1, ReplaceMessages: true})
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), writes)
	require.NoError(t, err)
	pushPreparedUsage(t, syncer)
	runPreparedUsageRefresh(t, store)
	// Exercise COALESCE's read contract for a stored empty display name.
	execPreparedUsage(t, store, `INSERT INTO sessions SELECT * REPLACE (toNullable('') AS display_name, push_version + 1 AS push_version) FROM sessions WHERE id = 'metadata-1'`)
	filter := db.UsageFilter{Timezone: "UTC", Project: "metadata"}
	_, before := usageQueryCounts(t, store)
	for _, limit := range []int{1, 3, 1} {
		got, err := store.GetTopSessionsByCost(t.Context(), filter, limit)
		require.NoError(t, err)
		require.Len(t, got, limit)
		assert.Equal(t, "metadata-0", got[0].SessionID)
		if limit == 3 {
			assert.Equal(t, "", got[1].DisplayName)
			assert.Equal(t, "session title", got[2].DisplayName)
		}
	}
	_, after := usageQueryCounts(t, store)
	assert.EqualValues(t, 1, after-before, "different limits reuse the narrow totals")
	var returned uint64
	require.NoError(t, store.DB().QueryRowContext(t.Context(), `
  SELECT sum(result_rows) FROM system.query_log
  WHERE current_database = currentDatabase() AND type = 'QueryFinish'
   AND position(query, 'FROM sessions s') > 0
   AND position(query, 'FROM usage_top_sessions') > 0`).Scan(&returned))
	assert.EqualValues(t, 5, returned, "metadata reads return only each request's winners")
	filter.TopSessionsSort = db.TopSessionsSortTokens
	filter.TopSessionsTokenTypes = db.UsageTokenTypeCacheRead
	got, err := store.GetTopSessionsByCost(t.Context(), filter, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "metadata-2", got[0].SessionID)
	assert.Equal(t, "metadata-1", got[1].SessionID)
	assert.Equal(t, "session title", got[0].DisplayName)
	assert.Equal(t, "", got[1].DisplayName)
	filter.Project = "absent"
	got, err = store.GetTopSessionsByCost(t.Context(), filter, 2)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestTopSessionsGroupedMetadata(t *testing.T) {
	t.Parallel()
	local, syncer, store := newPreparedUsageStore(t)
	var writes []db.SessionBatchWrite
	for i, tokens := range []int{1000000, 1, 500000, 250000, 100000} {
		id := fmt.Sprintf("group-metadata-%d", i)
		session := fixtureSession(id, "group-metadata", id, fmt.Sprintf("2026-01-12T03:0%d:00.000Z", i), 1)
		session.Agent = "hermes"
		if i < 4 {
			session.GroupKey = "daily:job"
		}
		if i == 1 {
			session.SessionName = new("Newest name · run")
		}
		if i == 3 {
			session.Project = "other-group-project"
		}
		writes = append(writes, db.SessionBatchWrite{Session: session, Messages: []db.Message{
			usagePriceMessage(id, 0, "2026-01-12T03:00:00.000Z", "round-test", fmt.Sprintf(`{"input_tokens":%d}`, tokens)),
		}, DataVersion: 1, ReplaceMessages: true})
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), append(writes[:2:2], writes[3:]...))
	require.NoError(t, err)
	pushPreparedUsage(t, syncer)
	other := dbtest.OpenTestDB(t)
	require.NoError(t, other.UpsertModelPricing([]db.ModelPricing{{ModelPattern: "round-test", InputPerMTok: money.MustParseDollars("0.4")}}))
	_, err = other.WriteSessionBatchAtomic(t.Context(), writes[2:3])
	require.NoError(t, err)
	otherSync, err := New(t.Context(), syncer.target, other, "other-machine", storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { otherSync.Close() })
	require.NoError(t, otherSync.EnsureSchema(t.Context()))
	pushPreparedUsage(t, otherSync)
	runPreparedUsageRefresh(t, store)
	filter := db.UsageFilter{Timezone: "UTC", Agent: "hermes", Project: "group-metadata,other-group-project", TopSessionsByGroup: true}
	got, err := store.GetTopSessionsByCost(t.Context(), filter, 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.EqualValues(t, 1000001, got[0].TotalTokens)
	assert.EqualValues(t, 850000, got[1].TotalTokens)
	assert.Empty(t, got[1].SessionID)
	got, err = store.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, "other-machine", got[1].Machine)
	assert.Equal(t, "other-group-project", got[2].Project)
	assert.Equal(t, "group-metadata-4", got[3].SessionID)
	execPreparedUsage(t, store, "SYSTEM FLUSH LOGS")
	var metadataReads uint64
	require.NoError(t, store.DB().QueryRowContext(t.Context(), `
		SELECT count() FROM system.query_log
		WHERE current_database = currentDatabase() AND type = 'QueryFinish'
			AND position(query, 'FROM usage_top_sessions') > 0`).Scan(&metadataReads))
	assert.EqualValues(t, 1, metadataReads, "grouped repeat reads reuse their display metadata")
	// A group change after a warm read must replace the grouped result.
	writes[1].Session.GroupKey = "different:job"
	_, err = local.WriteSessionBatchAtomic(t.Context(), writes[1:2])
	require.NoError(t, err)
	pushPreparedUsage(t, syncer)
	got, err = store.GetTopSessionsByCost(t.Context(), filter, 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "daily", got[0].DisplayName)
	assert.EqualValues(t, 1000000, got[0].TotalTokens)
}
