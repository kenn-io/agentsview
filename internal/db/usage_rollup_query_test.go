//go:build fts5

package db

import (
	"context"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

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

func countUsageRollupExceptionRows(t *testing.T, database *DB) int {
	t.Helper()
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	var count int
	require.NoError(t, cache.db.QueryRow(
		`SELECT COUNT(*) FROM usage_rollup_exceptions`).Scan(&count))
	return count
}

func TestUsageRollupUniqueClaudeSessionStoresNoExceptions(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "unique-claude", "project", func(session *Session) {
		session.Agent = "claude"
		session.StartedAt = &started
	})
	messages := make([]Message, 0, 6)
	for index := range 6 {
		messages = append(messages, Message{
			SessionID: "unique-claude", Ordinal: index, Role: "assistant",
			Timestamp: "2026-08-10T09:00:00Z", Model: "model-a",
			TokenUsage:      []byte(`{"input_tokens":3,"output_tokens":7}`),
			ClaudeMessageID: "message-" + strconv.Itoa(index),
			ClaudeRequestID: "request-" + strconv.Itoa(index),
		})
	}
	require.NoError(t, database.InsertMessages(messages))
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}

	legacy := getDailyUsageLegacyForRollupTest(t, database, filter)
	rollup := getDailyUsageRollupForTest(t, database, filter)

	assert.Equal(t, legacy, rollup)
	assert.Equal(t, 42, rollup.Totals.OutputTokens)
	assert.Zero(t, countUsageRollupExceptionRows(t, database),
		"unique Claude identities must aggregate into daily rows")
}

func TestUsageRollupSameDaySnapshotDuplicateAggregates(t *testing.T) {
	database := testDB(t)
	seedUsageSnapshotSession(t, database, "same-day", "project",
		"2026-08-10T09:00:00Z", 0, 10, "model-a")
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "same-day", Ordinal: 1, Role: "assistant",
		Timestamp: "2026-08-10T09:05:00Z", Model: "model-a",
		TokenUsage:      []byte(`{"output_tokens":25}`),
		ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
	}}))
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}

	legacy := getDailyUsageLegacyForRollupTest(t, database, filter)
	rollup := getDailyUsageRollupForTest(t, database, filter)

	assert.Equal(t, legacy, rollup)
	assert.Equal(t, 25, rollup.Totals.OutputTokens,
		"only the winning snapshot output may count")
	assert.Zero(t, countUsageRollupExceptionRows(t, database),
		"a same-day, same-session snapshot group must finalize at build time")
}

func TestUsageRollupSiblingArrivalInvalidatesFinalizedAggregate(t *testing.T) {
	database := testDB(t)
	seedUsageSnapshotSession(t, database, "original", "project-a",
		"2026-08-10T09:00:00Z", 0, 10, "model-a")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	warm := getDailyUsageRollupForTest(t, database, filter)
	require.Equal(t, 10, warm.Totals.OutputTokens)
	require.Zero(t, countUsageRollupExceptionRows(t, database))

	seedUsageSnapshotSession(t, database, "sibling", "project-b",
		"2026-08-10T10:00:00Z", 0, 20, "model-a")

	legacy := getDailyUsageLegacyForRollupTest(t, database, filter)
	rollup := getDailyUsageRollupForTest(t, database, filter)
	assert.Equal(t, legacy, rollup,
		"a new cross-session sibling must invalidate the finalized aggregate")
	assert.Equal(t, 20, rollup.Totals.OutputTokens)
}

func TestUsageRollupSiblingRemovalRestoresFinalizedAggregate(t *testing.T) {
	database := testDB(t)
	seedUsageSnapshotSession(t, database, "original", "project-a",
		"2026-08-10T09:00:00Z", 0, 10, "model-a")
	seedUsageSnapshotSession(t, database, "sibling", "project-b",
		"2026-08-10T10:00:00Z", 0, 20, "model-a")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	warm := getDailyUsageRollupForTest(t, database, filter)
	require.Equal(t, 20, warm.Totals.OutputTokens)
	require.Positive(t, countUsageRollupExceptionRows(t, database))

	require.NoError(t, database.ReplaceSessionMessages("sibling", []Message{{
		SessionID: "sibling", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T10:00:00Z", Model: "model-a",
		TokenUsage:      []byte(`{"output_tokens":20}`),
		ClaudeMessageID: "unrelated-message", ClaudeRequestID: "unrelated-request",
	}}))

	legacy := getDailyUsageLegacyForRollupTest(t, database, filter)
	rollup := getDailyUsageRollupForTest(t, database, filter)
	assert.Equal(t, legacy, rollup)
	assert.Equal(t, 30, rollup.Totals.OutputTokens,
		"both requests must count once the identities no longer collide")
	assert.Zero(t, countUsageRollupExceptionRows(t, database),
		"separated identities must become daily rows again")
}

func TestUsageRollupSessionDeletionRestoresFinalizedAggregate(t *testing.T) {
	database := testDB(t)
	seedUsageSnapshotSession(t, database, "original", "project-a",
		"2026-08-10T09:00:00Z", 0, 10, "model-a")
	seedUsageSnapshotSession(t, database, "sibling", "project-b",
		"2026-08-10T10:00:00Z", 0, 20, "model-a")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	warm := getDailyUsageRollupForTest(t, database, filter)
	require.Equal(t, 20, warm.Totals.OutputTokens)

	require.NoError(t, database.DeleteSession("sibling"))

	legacy := getDailyUsageLegacyForRollupTest(t, database, filter)
	rollup := getDailyUsageRollupForTest(t, database, filter)
	assert.Equal(t, legacy, rollup)
	assert.Equal(t, 10, rollup.Totals.OutputTokens,
		"a deleted sibling must stop competing immediately")
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

func TestUsageRollupCrossIdentityQueriesUseIndexes(t *testing.T) {
	database := testDB(t)
	seedUsageSnapshotSession(t, database, "plan", "project",
		"2026-08-10T09:00:00Z", 0, 10, "model-a")
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	conn, err := cache.db.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `CREATE TEMP TABLE IF NOT EXISTS
		usage_rollup_build_sessions(session_id TEXT PRIMARY KEY) WITHOUT ROWID`)
	require.NoError(t, err)

	plan := func(query string) string {
		t.Helper()
		rows, err := conn.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+query)
		require.NoError(t, err)
		defer rows.Close()
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			details = append(details, detail)
		}
		require.NoError(t, rows.Err())
		return strings.Join(details, "\n")
	}
	assert.Contains(t, plan(usageRollupCrossSnapshotSQL),
		"usage_facts_claude_identity")
	assert.Contains(t, plan(usageRollupCrossSourceUUIDSQL),
		"usage_facts_source_uuid")
	usageKeyPlan := plan(usageRollupCrossUsageKeySQL)
	assert.Contains(t, usageKeyPlan, "usage_facts_usage_dedup_key")
	assert.Contains(t, usageKeyPlan, "cursor_usage_facts_dedup_key")
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
	sameDayStarted := "2026-11-01T09:00:00Z"
	insertSession(t, database, "claude-c", "project-a", func(session *Session) {
		session.Agent = "claude"
		session.StartedAt = &sameDayStarted
	})
	sameDayMessage := func(ordinal, output int, messageID string) Message {
		return Message{
			SessionID: "claude-c", Ordinal: ordinal, Role: "assistant",
			Timestamp: "2026-11-01T09:0" + strconv.Itoa(ordinal) + ":00Z",
			Model:     "model-a",
			TokenUsage: []byte(`{"input_tokens":10,"output_tokens":` +
				strconv.Itoa(output) + `}`),
			ClaudeMessageID: messageID, ClaudeRequestID: "same-day-request",
		}
	}
	require.NoError(t, database.InsertMessages([]Message{
		sameDayMessage(0, 11, "same-day-message"),
		sameDayMessage(1, 44, "same-day-message"),
		sameDayMessage(2, 7, "same-day-unique"),
	}))
	copilotStarted := "2026-11-01T10:00:00Z"
	insertSession(t, database, "copilot-a", "project-b", func(session *Session) {
		session.Agent = "copilot"
		session.StartedAt = &copilotStarted
	})
	firstShutdown := money.MustParseDollars("4.00")
	lastShutdown := money.MustParseDollars("6.50")
	require.NoError(t, database.ReplaceSessionUsageEvents("copilot-a", []UsageEvent{
		{Source: "shutdown", Model: "model-a", InputTokens: 15,
			Cost: &firstShutdown, CostStatus: "exact",
			CostSource: CopilotReportedCostSource,
			OccurredAt: "2026-11-01T11:00:00Z", DedupKey: "shutdown-one"},
		{Source: "shutdown", Model: "model-b", InputTokens: 25,
			Cost: &lastShutdown, CostStatus: "exact",
			CostSource: CopilotReportedCostSource,
			OccurredAt: "2026-11-02T11:00:00Z", DedupKey: "shutdown-two"},
	}))
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
	agents := []string{"", "claude", "codex", "cursor", "copilot"}
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

func TestUsageRollupQueryRejectsDifferentInstallOrPricing(t *testing.T) {
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
		SET install_revision = ? WHERE id = ?`,
		required.InstallRevision-1, required.ID)
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

func TestUsageRollupInstallWaitsForHeldCacheWriteLock(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "model-a",
		InputPerMTok:  money.MustParseDollars("1.0"),
		OutputPerMTok: money.MustParseDollars("2.0"),
	}}))
	seedUsageSnapshotSession(t, database, "held-session", "proj",
		"2026-08-10T09:00:00Z", 0, 10, "model-a")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	warm, err := database.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.Len(t, warm.Daily, 1)

	// A pricing change leaves facts current but forces a rollup
	// reinstall on the next read.
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "model-a",
		InputPerMTok:  money.MustParseDollars("1.0"),
		OutputPerMTok: money.MustParseDollars("4.0"),
	}}))
	databaseID, err := database.GetDatabaseID(ctx)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(ctx, databaseID)
	require.NoError(t, err)

	// Hold the cache write lock on a separate connection while the
	// query runs: the rollup install must wait it out through the busy
	// timeout instead of failing with a stale-snapshot write error.
	held := openRawUsageCacheTestDB(t, cache.path)
	t.Cleanup(func() { require.NoError(t, held.Close()) })
	_, err = held.Exec(`BEGIN IMMEDIATE`)
	require.NoError(t, err)
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(300 * time.Millisecond)
		_, _ = held.Exec(`COMMIT`)
	}()

	result, err := database.GetDailyUsage(ctx, filter)
	<-released
	require.NoError(t, err,
		"rollup install must wait for the held cache write lock")
	require.Len(t, result.Daily, 1)
	assert.Equal(t, 10, result.Daily[0].OutputTokens)
}
