//go:build fts5

package db

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTopSessionsRollupMatchesLegacy(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	ctx := context.Background()
	for _, filter := range []UsageFilter{
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC",
			TopSessionsSort:       TopSessionsSortTokens,
			TopSessionsTokenTypes: UsageTokenTypeInput | UsageTokenTypeOutput},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC",
			Project: "proj-a", Model: "model-a"},
	} {
		legacy, err := database.getTopSessionsByCostLegacy(ctx, filter, 3)
		require.NoError(t, err)
		facts, err := database.GetTopSessionsByCost(ctx, filter, 3)
		require.NoError(t, err)
		assert.Equal(t, legacy, facts)
	}
}

func TestUsageSessionCountsRollupMatchesLegacy(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	ctx := context.Background()
	for _, filter := range []UsageFilter{
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"},
		{From: "2024-06-01", To: "2024-06-30", Timezone: "UTC",
			Agent: "claude", Model: "gpt-5"},
	} {
		legacy, err := database.getUsageSessionCountsLegacy(ctx, filter)
		require.NoError(t, err)
		facts, err := database.GetUsageSessionCounts(ctx, filter)
		require.NoError(t, err)
		assert.Equal(t, legacy, facts)
	}
}

func TestUsageMatchingSessionCountRollupMatchesLegacy(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "relaxed", "project", func(session *Session) {
		session.Agent = "copilot"
		session.StartedAt = Ptr("2026-08-10T08:00:00Z")
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "relaxed", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "",
	}}))
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	legacy, err := database.getUsageMatchingSessionCountLegacy(
		context.Background(), filter)
	require.NoError(t, err)
	facts, err := database.GetUsageMatchingSessionCount(
		context.Background(), filter)
	require.NoError(t, err)
	assert.Equal(t, legacy, facts)
}

func TestUsageMatchingSessionCountRollupCountsDuplicateActivityOwners(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"first", "second"} {
		insertSession(t, database, id, "project", func(session *Session) {
			session.Agent = "claude"
			session.StartedAt = Ptr("2026-08-10T08:00:00Z")
		})
		require.NoError(t, database.InsertMessages([]Message{{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-10T09:00:00Z", Model: "model",
			TokenUsage:      json.RawMessage(`{"input_tokens":1,"output_tokens":1}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		}}))
	}
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	legacy, err := database.getUsageMatchingSessionCountLegacy(
		context.Background(), filter)
	require.NoError(t, err)
	facts, err := database.GetUsageMatchingSessionCount(
		context.Background(), filter)
	require.NoError(t, err)
	assert.Equal(t, legacy, facts)
}

func TestUsageMatchingSessionCountRollupCountsUsageEventSessions(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"first-event", "second-event"} {
		insertSession(t, database, id, "project", func(session *Session) {
			session.Agent = "copilot"
			session.StartedAt = Ptr("2026-08-10T08:00:00Z")
		})
		require.NoError(t, database.ReplaceSessionUsageEvents(id, []UsageEvent{{
			Source: "session", Model: "model", InputTokens: 1,
			OccurredAt: "2026-08-10T09:00:00Z", DedupKey: "event",
		}}))
	}
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	legacy, err := database.getUsageMatchingSessionCountLegacy(
		context.Background(), filter)
	require.NoError(t, err)
	require.Equal(t, 2, legacy)
	facts, err := database.GetUsageMatchingSessionCount(
		context.Background(), filter)
	require.NoError(t, err)
	assert.Equal(t, legacy, facts)
}

func TestDailyUsageFactsDoesNotDeduplicateSourceUUIDWithoutAgent(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "blank-agent", "project", func(session *Session) {
		session.Agent = ""
		session.StartedAt = Ptr("2026-08-10T08:00:00Z")
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "blank-agent", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "model",
		TokenUsage: []byte(`{"input_tokens":3,"output_tokens":4}`),
		SourceUUID: "source",
	}}))
	filter := UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
		SkipSessionCounts: true,
	}
	legacy, err := database.getDailyUsageLegacy(context.Background(), filter)
	require.NoError(t, err)
	facts, err := database.GetDailyUsage(context.Background(), filter)
	require.NoError(t, err)
	assert.Equal(t, legacy.Totals, facts.Totals)
}
