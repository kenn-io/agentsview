//go:build fts5

package db

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTopSessionsRollupMatchesLegacy(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	ctx := t.Context()
	for _, filter := range []UsageFilter{
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"},
		{
			From: "2024-06-01", To: "2024-07-31", Timezone: "UTC",
			TopSessionsSort:       TopSessionsSortTokens,
			TopSessionsTokenTypes: UsageTokenTypeInput | UsageTokenTypeOutput,
		},
		{
			From: "2024-06-01", To: "2024-07-31", Timezone: "UTC",
			Project: "proj-a", Model: "model-a",
		},
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
	ctx := t.Context()
	for _, filter := range []UsageFilter{
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"},
		{
			From: "2024-06-01", To: "2024-06-30", Timezone: "UTC",
			Agent: "claude", Model: "gpt-5",
		},
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
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "relaxed", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "",
	}}))
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	legacy, err := database.getUsageMatchingSessionCountLegacy(
		t.Context(), filter)
	require.NoError(t, err)
	facts, err := database.GetUsageMatchingSessionCount(
		t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, legacy, facts)
}

func TestUsageMatchingSessionCountRollupPreservesUndatedActivity(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "undated", "project")
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "undated", Ordinal: 0, Role: "assistant",
	}}))

	ctx := t.Context()
	unbounded := UsageFilter{}
	legacy, err := database.getUsageMatchingSessionCountLegacy(ctx, unbounded)
	require.NoError(t, err)
	require.Equal(t, 1, legacy)
	got, err := database.GetUsageMatchingSessionCount(ctx, unbounded)
	require.NoError(t, err)
	assert.Equal(t, legacy, got)

	bounded := UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	}
	legacy, err = database.getUsageMatchingSessionCountLegacy(ctx, bounded)
	require.NoError(t, err)
	require.Zero(t, legacy)
	got, err = database.GetUsageMatchingSessionCount(ctx, bounded)
	require.NoError(t, err)
	assert.Equal(t, legacy, got)
}

func TestUsageMatchingSessionCountRollupCountsDuplicateActivityOwners(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"first", "second"} {
		insertSession(t, database, id, "project", func(session *Session) {
			session.Agent = "claude"
			session.StartedAt = Ptr("2026-08-10T08:00:00Z")
		})
		require.NoError(t, database.InsertMessages(t.Context(), []Message{{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-10T09:00:00Z", Model: "model",
			TokenUsage:      json.RawMessage(`{"input_tokens":1,"output_tokens":1}`),
			ClaudeMessageID: "shared-message", ClaudeRequestID: "shared-request",
		}}))
	}
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	legacy, err := database.getUsageMatchingSessionCountLegacy(
		t.Context(), filter)
	require.NoError(t, err)
	facts, err := database.GetUsageMatchingSessionCount(
		t.Context(), filter)
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
		require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), id, []UsageEvent{{
			Source: "session", Model: "model", InputTokens: 1,
			OccurredAt: "2026-08-10T09:00:00Z", DedupKey: "event",
		}}))
	}
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	legacy, err := database.getUsageMatchingSessionCountLegacy(
		t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, 2, legacy)
	facts, err := database.GetUsageMatchingSessionCount(
		t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, legacy, facts)
}

func TestDailyUsageFactsDoesNotDeduplicateSourceUUIDWithoutAgent(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "blank-agent", "project", func(session *Session) {
		session.Agent = ""
		session.StartedAt = Ptr("2026-08-10T08:00:00Z")
	})
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "blank-agent", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "model",
		TokenUsage: []byte(`{"input_tokens":3,"output_tokens":4}`),
		SourceUUID: "source",
	}}))
	filter := UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
		SkipSessionCounts: true,
	}
	legacy, err := database.getDailyUsageLegacy(t.Context(), filter)
	require.NoError(t, err)
	facts, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, legacy.Totals, facts.Totals)
}

func TestUsageWindowMemoPreparesOncePerPredicate(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2024-06-10T09:00:00Z", 0, 10)
	_, err := database.getWriter().Exec(t.Context(), `UPDATE sessions SET user_message_count = 55296 WHERE id = 'reported'`)
	require.NoError(t, err)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	ctx := WithUsageWindowMemo(t.Context())
	filters := []UsageFilter{
		{From: "2024-06-01", To: "2024-07-31", Timezone: "America/Chicago"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "America/Chicago", Model: "model-a"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "America/Chicago", Project: "proj-a"},
	}
	for _, filter := range filters {
		want := getDailyUsageLegacyForRollupTest(t, database, filter)
		got, err := database.getDailyUsageUncached(ctx, filter)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	assert.Equal(t, int32(2), prepares.Load())
	// These adjacent surrogate code points collide when integers are encoded as runes.
	for _, minimum := range []int{55296, 55297} {
		filter := UsageFilter{From: "2024-06-01", To: "2024-07-31", Timezone: "America/Chicago", MinUserMessages: minimum}
		want := getDailyUsageLegacyForRollupTest(t, database, filter)
		got, err := database.getDailyUsageUncached(ctx, filter)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	assert.Equal(t, int32(4), prepares.Load())
}

func TestUsageWindowMemoSeparatesCursorPreparation(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 10)
	require.NoError(t, database.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{OccurredAt: "2026-08-10T09:00:00Z", Model: "cursor-model", InputTokens: 7, DedupKey: "cursor"}}))
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	ctx := WithUsageWindowMemo(t.Context())
	// Exercise the memo boundary directly; Pairwise only reads daily usage.
	_, err := database.sharedUsageWindow(ctx, usageWindowKey(filter, usageQueryKindToken, false), filter, usageQueryKindToken, false)
	require.NoError(t, err)
	daily, err := database.getDailyUsageUncached(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 17, daily.Totals.InputTokens)
	assert.Equal(t, int32(2), prepares.Load(), "daily usage needs its own preparation to include Cursor")
}

func TestUsageWindowMemoRecapturesAfterInstallChange(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	filter := UsageFilter{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"}
	want := getDailyUsageLegacyForRollupTest(t, database, filter)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	ctx := WithUsageWindowMemo(t.Context())
	first, err := database.getDailyUsageUncached(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, want, first)
	snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.db.ExecContext(t.Context(), `UPDATE usage_rollup_installs SET install_revision = install_revision + 1 WHERE id = (SELECT MIN(id) FROM usage_rollup_installs)`)
	require.NoError(t, err)
	second, err := database.getDailyUsageUncached(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, want, second)
	assert.Equal(t, int32(2), prepares.Load())
	// The recapture bypassed the memo, so this read prepares a new shared window.
	third, err := database.getDailyUsageUncached(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, want, third)
	assert.Equal(t, int32(3), prepares.Load())
}

func TestUsageWindowMemoSharesConcurrentSides(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	filter := UsageFilter{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"}
	wants := make(map[string]DailyUsageResult)
	for _, model := range []string{"model-a", "model-b"} {
		side := filter
		side.Model = model
		wants[model] = getDailyUsageLegacyForRollupTest(t, database, side)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var readers sync.WaitGroup
	defer readers.Wait()
	defer unblock()
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) {
		if prepares.Add(1) == 1 {
			close(entered)
		}
		<-release
	})
	ctx := WithUsageWindowMemo(t.Context())
	type result struct {
		model string
		daily DailyUsageResult
		err   error
	}
	done := make(chan result, 2)
	read := func(model string) {
		readers.Go(func() {
			side := filter
			side.Model = model
			daily, err := database.getDailyUsageUncached(ctx, side)
			done <- result{model, daily, err}
		})
	}
	read("model-a")
	<-entered

	read("model-b")
	unblock()
	for range 2 {
		got := <-done
		require.NoError(t, got.err)
		assert.Equal(t, wants[got.model], got.daily)
	}
	assert.Equal(t, int32(1), prepares.Load())
}

func TestUsageWindowMemoRecapturesAfterGenerationRetirement(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	filter := UsageFilter{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"}
	want := getDailyUsageLegacyForRollupTest(t, database, filter)
	var prepares atomic.Int32
	database.SetUsageWindowPrepareForTest(func(context.Context) { prepares.Add(1) })
	ctx := WithUsageWindowMemo(t.Context())
	first, err := database.getDailyUsageUncached(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, want, first)
	assert.Equal(t, int32(1), prepares.Load())

	require.NoError(t, database.SetDatabaseIDForTest(t.Context(), "replacement-database-id"))
	require.NoError(t, database.usageCache.RetireExcept("replacement-database-id"))
	second, err := database.getDailyUsageUncached(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, want, second)
	assert.Equal(t, int32(2), prepares.Load())
}
