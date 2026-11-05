//go:build fts5

package db

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	var prepares atomic.Int32
	database.usageWindowObserver.beforePrepare = func(context.Context) { prepares.Add(1) }
	ctx := WithUsageWindowMemo(t.Context())
	filters := []UsageFilter{
		{From: "2024-06-01", To: "2024-07-31", Timezone: "America/Chicago"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "America/Chicago", Model: "model-a"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "America/Chicago", Project: "proj-a"},
	}
	for _, filter := range filters {
		want := getDailyUsageLegacyForRollupTest(t, database, filter)
		got, err := database.GetDailyUsage(ctx, filter)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	want, err := database.getTopSessionsByCostLegacy(t.Context(), filters[0], 3)
	require.NoError(t, err)
	got, err := database.GetTopSessionsByCost(ctx, filters[0], 3)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, int32(2), prepares.Load())
}

func TestConcurrentUsageReadsShareOneWindow(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	filters := []UsageFilter{
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC", Model: "model-b"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC", Model: "model-a"},
	}
	wants := make([]DailyUsageResult, len(filters))
	for i, filter := range filters {
		wants[i] = getDailyUsageLegacyForRollupTest(t, database, filter)
	}
	wantTop, err := database.getTopSessionsByCostLegacy(t.Context(), filters[0], 3)
	require.NoError(t, err)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var prepares atomic.Int32
	database.usageWindowObserver = usageWindowObserver{
		beforePrepare: func(context.Context) { prepares.Add(1); <-release },
	}
	type dailyResult struct {
		daily DailyUsageResult
		err   error
	}
	done := make([]chan dailyResult, len(filters))
	canceledCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for i, filter := range filters {
		done[i] = make(chan dailyResult, 1)
		ctx := t.Context()
		if i == 0 {
			ctx = canceledCtx
		}
		go func() {
			daily, err := database.GetDailyUsage(ctx, filter)
			done[i] <- dailyResult{daily, err}
		}()
	}
	type topResult struct {
		top []TopSessionEntry
		err error
	}
	topDone := make(chan topResult, 1)
	go func() {
		top, err := database.GetTopSessionsByCost(t.Context(), filters[0], 3)
		topDone <- topResult{top, err}
	}()
	key := usageWindowKey(filters[0], usageQueryKindToken, usageCursorIncluded(filters[0]))
	require.Eventually(t, func() bool { return database.usageWindows.Waiters(key) == 4 }, 10*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, (<-done[0]).err, context.Canceled)
	unblock()
	for i := 1; i < len(done); i++ {
		got := <-done[i]
		require.NoError(t, got.err)
		assert.Equal(t, wants[i], got.daily)
	}
	top := <-topDone
	require.NoError(t, top.err)
	assert.Equal(t, wantTop, top.top)
	assert.Equal(t, int32(1), prepares.Load())
}

func TestUsageWindowMemoRecapturesAfterInstallChange(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	filter := UsageFilter{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"}
	want := getDailyUsageLegacyForRollupTest(t, database, filter)
	var prepares atomic.Int32
	database.usageWindowObserver.beforePrepare = func(context.Context) { prepares.Add(1) }
	ctx := WithUsageWindowMemo(t.Context())
	first, err := database.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, want, first)
	snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.db.ExecContext(t.Context(), `UPDATE usage_rollup_installs SET install_revision = install_revision + 1 WHERE id = (SELECT MIN(id) FROM usage_rollup_installs)`)
	require.NoError(t, err)
	second, err := database.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, want, second)
	assert.Equal(t, int32(2), prepares.Load())
	// The fresh preparation replaces the stale memo for later reads too.
	third, err := database.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, want, third)
	assert.Equal(t, int32(2), prepares.Load())
}

func TestConcurrentUsageReadsCancelAbandonedWindow(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	filter := UsageFilter{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"}
	want := getDailyUsageLegacyForRollupTest(t, database, filter)
	canceled := make(chan struct{})
	var prepares atomic.Int32
	database.usageWindowObserver = usageWindowObserver{
		beforePrepare: func(ctx context.Context) {
			if prepares.Add(1) == 1 {
				<-ctx.Done()
				close(canceled)
			}
		},
	}
	cancels := make([]context.CancelFunc, 4)
	done := make(chan error, 4)
	for i := range cancels {
		ctx, cancel := context.WithCancel(t.Context())
		cancels[i] = cancel
		t.Cleanup(cancel)
		go func() {
			_, err := database.GetDailyUsage(ctx, filter)
			done <- err
		}()
	}
	key := usageWindowKey(filter, usageQueryKindToken, usageCursorIncluded(filter))
	require.Eventually(t, func() bool { return database.usageWindows.Waiters(key) == 4 }, 10*time.Second, time.Millisecond)
	for _, cancel := range cancels {
		cancel()
	}
	for range 4 {
		require.ErrorIs(t, <-done, context.Canceled)
	}
	<-canceled
	// A later request must start new work instead of joining the canceled build.
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, int32(2), prepares.Load())
}
