//go:build fts5

package db

import (
	"math/rand"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
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

func TestUsageRollupDailyMatchesCursorAutomatedScope(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.InsertCursorUsageEvents([]CursorUsageEvent{
		{
			OccurredAt: "2026-08-10T09:00:00Z", Model: "model-a",
			InputTokens: 10, Charged: money.MustParseDollars("0.10"),
			DedupKey: "interactive", IsHeadless: false,
		},
		{
			OccurredAt: "2026-08-10T10:00:00Z", Model: "model-a",
			InputTokens: 20, Charged: money.MustParseDollars("0.70"),
			DedupKey: "headless", IsHeadless: true,
		},
	}))

	for _, filter := range []UsageFilter{
		{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
			ExcludeAutomated: true},
		{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
			AutomatedScope: "automated"},
	} {
		legacy := getDailyUsageLegacyForRollupTest(t, database, filter)
		rollup := getDailyUsageRollupForTest(t, database, filter)
		assert.Equal(t, legacy, rollup)
	}
}

func TestUsageRollupDailyMatchesLastCopilotReportedCost(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "copilot:reported", "project", func(session *Session) {
		session.Agent = "copilot"
		session.StartedAt = Ptr("2026-08-10T08:00:00Z")
	})
	first := money.MustParseDollars("5.00")
	last := money.MustParseDollars("3.00")
	require.NoError(t, database.ReplaceSessionUsageEvents(
		"copilot:reported", []UsageEvent{
			{
				Source: "shutdown", Model: "model-a", InputTokens: 10,
				Cost: &first, CostStatus: "exact", CostSource: CopilotReportedCostSource,
				OccurredAt: "2026-08-10T09:00:00Z", DedupKey: "first",
			},
			{
				Source: "shutdown", Model: "model-a", InputTokens: 20,
				Cost: &last, CostStatus: "exact", CostSource: CopilotReportedCostSource,
				OccurredAt: "2026-08-10T10:00:00Z", DedupKey: "last",
			},
		},
	))
	filter := UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	}

	legacy := getDailyUsageLegacyForRollupTest(t, database, filter)
	rollup := getDailyUsageRollupForTest(t, database, filter)
	assert.Equal(t, money.MustParseDollars("3.00"), legacy.Totals.TotalCost)
	assert.Equal(t, legacy, rollup)
}

func TestUsageRollupWarmReadDoesNotScanFacts(t *testing.T) {
	database := testDB(t)
	seedUsageSnapshotSession(t, database, "warm", "project",
		"2026-08-10T09:00:00Z", 0, 10, "model-a")
	filter := UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	}
	want, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err, "warm rollup")
	snapshot, err := database.captureUsageQuery(
		t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.db.Exec(`DROP TABLE usage_facts`)
	require.NoError(t, err, "make any facts access fail")

	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err, "read warmed rollup")
	assert.Equal(t, want, got)
}

func TestUsageRollupSeededRandomParitySweep(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{
		{ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("2"),
			OutputPerMTok: money.MustParseDollars("8")},
		{ModelPattern: "model-b", InputPerMTok: money.MustParseDollars("3"),
			OutputPerMTok: money.MustParseDollars("12")},
	}))

	type seededSession struct {
		id, project, agent, timestamp, model string
		automated                            bool
	}
	fixtures := []seededSession{
		{"claude-a", "project-a", "claude", "2026-10-31T23:30:00Z", "model-a", false},
		{"claude-b", "project-b", "claude", "2026-11-01T07:30:00Z", "model-a", true},
		{"codex-a", "project-a", "codex", "2026-11-01T06:30:00Z", "model-a", false},
		{"codex-b", "project-b", "codex", "2026-11-02T01:30:00Z", "model-b", true},
		{"plain-a", "project-a", "openhands", "2026-11-03T12:00:00Z", "model-b", false},
	}
	for index, fixture := range fixtures {
		started := fixture.timestamp
		insertSession(t, database, fixture.id, fixture.project, func(session *Session) {
			session.Agent = fixture.agent
			session.StartedAt = &started
			session.IsAutomated = fixture.automated
		})
		message := Message{
			SessionID: fixture.id, Ordinal: 0, Role: "assistant",
			Timestamp: fixture.timestamp, Model: fixture.model,
			TokenUsage: []byte(`{"input_tokens":100,"output_tokens":25}`),
		}
		switch fixture.agent {
		case "claude":
			message.ClaudeMessageID = "cross-day-message"
			message.ClaudeRequestID = "cross-day-request"
			message.OutputTokens = 25 + index
			message.HasOutputTokens = true
		case "codex":
			message.SourceUUID = "cross-session-source"
		}
		require.NoError(t, database.InsertMessages([]Message{message}))
	}
	// Keep automation deterministic regardless of transcript classifier rules.
	_, err := database.getWriter().Exec(`
		UPDATE sessions SET is_automated = CASE id
			WHEN 'claude-b' THEN 1 WHEN 'codex-b' THEN 1 ELSE 0 END`)
	require.NoError(t, err)
	reported := money.MustParseDollars("0.25")
	require.NoError(t, database.ReplaceSessionUsageEvents("plain-a", []UsageEvent{{
		Source: "provider", Model: "model-b", InputTokens: 50,
		Cost: &reported, CostStatus: "exact", CostSource: "provider",
		OccurredAt: "2026-11-01T05:30:00Z", DedupKey: "event-one",
	}}))
	require.NoError(t, database.InsertCursorUsageEvents([]CursorUsageEvent{
		{OccurredAt: "2026-11-01T06:15:00Z", Model: "model-a",
			InputTokens: 30, DedupKey: "cursor-human", IsHeadless: false},
		{OccurredAt: "2026-11-01T07:15:00Z", Model: "model-b",
			InputTokens: 40, DedupKey: "cursor-headless", IsHeadless: true},
	}))
	for _, filter := range []UsageFilter{
		{From: "2026-11-01", To: "2026-11-01", Timezone: "America/Chicago"},
		{From: "2026-10-31", To: "2026-11-02", Timezone: "UTC",
			Project: "project-a", Model: "model-a"},
	} {
		legacy := getDailyUsageLegacyForRollupTest(t, database, filter)
		rollup := getDailyUsageRollupForTest(t, database, filter)
		assert.Equal(t, legacy, rollup, "required filter %#v", filter)
	}

	random := rand.New(rand.NewSource(1454)) //nolint:gosec // deterministic test sweep
	from := []string{"2026-10-31", "2026-11-01", "2026-11-02", ""}
	to := []string{"2026-11-01", "2026-11-02", "2026-11-03", ""}
	zones := []string{"UTC", "America/Chicago", "Asia/Tokyo"}
	projects := []string{"", "project-a", "project-b"}
	agents := []string{"", "claude", "codex", "cursor"}
	models := []string{"", "model-a", "model-b"}
	scopes := []string{"", "human", "automated"}
	for iteration := range 36 {
		fromIndex := random.Intn(len(from))
		toIndex := random.Intn(len(to))
		filter := UsageFilter{
			From: from[fromIndex], To: to[toIndex],
			Timezone:       zones[random.Intn(len(zones))],
			Project:        projects[random.Intn(len(projects))],
			Agent:          agents[random.Intn(len(agents))],
			Model:          models[random.Intn(len(models))],
			AutomatedScope: scopes[random.Intn(len(scopes))],
		}
		if filter.From != "" && filter.To != "" && filter.From > filter.To {
			filter.From, filter.To = filter.To, filter.From
		}
		legacy := getDailyUsageLegacyForRollupTest(t, database, filter)
		rollup := getDailyUsageRollupForTest(t, database, filter)
		assert.Equal(t, legacy, rollup, "iteration %d filter %#v", iteration, filter)
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
