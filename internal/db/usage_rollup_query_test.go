//go:build fts5

package db

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

func TestUsageRollupDailyMatchesFacts(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	for _, filter := range []UsageFilter{
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC",
			Project: "proj-a", Model: "model-a"},
		{From: "2024-06-01", To: "2024-06-30", Timezone: "America/Chicago",
			ExcludeAgent: "codex"},
	} {
		facts := getDailyUsageLegacyForRollupTest(t, database, filter)
		rollup := getDailyUsageRollupForTest(t, database, filter)
		assert.Equal(t, facts, rollup)
	}
}

func seedUsageSnapshotSession(
	t *testing.T, database *DB, id, project, timestamp string,
	ordinal, output int, model string,
) {
	t.Helper()
	started := timestamp
	insertSession(t, database, id, project, func(session *Session) {
		session.StartedAt = &started
		session.Agent = "claude"
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: id, Ordinal: ordinal, Role: "assistant", Timestamp: timestamp,
		Model: model, TokenUsage: []byte(
			`{"output_tokens":` + strconv.Itoa(output) + `}`),
		ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
	}}))
}

func TestUsageRollupDailyMatchesFactsForCrossSessionSnapshots(t *testing.T) {
	database := testDB(t)
	seedUsageSnapshotSession(t, database, "earlier", "project-a",
		"2026-08-10T09:00:00Z", 0, 10, "model-a")
	seedUsageSnapshotSession(t, database, "later", "project-b",
		"2026-08-10T10:00:00Z", 0, 20, "model-a")
	for _, filter := range []UsageFilter{
		{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"},
		{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
			Project: "project-a"},
		{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
			Project: "project-b"},
	} {
		facts := getDailyUsageLegacyForRollupTest(t, database, filter)
		rollup := getDailyUsageRollupForTest(t, database, filter)
		assert.Equal(t, facts, rollup)
	}
}

func TestUsageRollupQueryRecapturesAfterConcurrentInstall(t *testing.T) {
	database := testDB(t)
	seedUsageSnapshotSession(t, database, "session-a", "project-a",
		"2026-08-10T09:00:00Z", 0, 10, "model-a")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	fills, err := cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(t, err)
	resolver := export.NewPricingResolver(snapshot.PricingRows)
	installs, _, err := cache.rollup.Ensure(t.Context(), snapshot, fills, resolver)
	require.NoError(t, err)
	required := installs["session-a"]
	_, err = cache.db.Exec(`UPDATE usage_rollup_installs
		SET install_revision = install_revision + 1 WHERE id = ?`, required.ID)
	require.NoError(t, err)

	_, err = cache.usageRollupQuery(t.Context(), snapshot, filter, installs, resolver)
	assert.ErrorIs(t, err, errUsageCacheSourceChanged)
	_, err = cache.db.Exec(`UPDATE usage_rollup_installs
		SET install_revision = ?, pricing_hash = 'changed' WHERE id = ?`,
		required.InstallRevision, required.ID)
	require.NoError(t, err)
	_, err = cache.usageRollupQuery(t.Context(), snapshot, filter, installs, resolver)
	assert.ErrorIs(t, err, errUsageCacheSourceChanged)
}

func getDailyUsageLegacyForRollupTest(
	t *testing.T, database *DB, filter UsageFilter,
) DailyUsageResult {
	t.Helper()
	daily, err := database.getDailyUsageLegacy(t.Context(), filter)
	require.NoError(t, err)
	return daily
}

func getDailyUsageRollupForTest(
	t *testing.T, database *DB, filter UsageFilter,
) DailyUsageResult {
	t.Helper()
	snapshot, err := database.captureUsageQuery(
		t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	fills, err := cache.fill.Ensure(
		t.Context(), snapshot.Versions, snapshot.CursorHighWater)
	require.NoError(t, err)
	snapshot.dropDeleted(fills)
	resolver := export.NewPricingResolver(snapshot.PricingRows)
	installs, _, err := cache.rollup.Ensure(
		t.Context(), snapshot, fills, resolver)
	require.NoError(t, err)
	result, err := cache.usageRollupQuery(
		t.Context(), snapshot, filter, installs, resolver)
	require.NoError(t, err)
	daily, err := database.assembleDailyUsageFacts(
		t.Context(), filter, result, resolver)
	require.NoError(t, err)
	return daily
}
