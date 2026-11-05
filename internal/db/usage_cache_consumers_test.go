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

	"go.kenn.io/agentsview/internal/money"
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
	for _, readOnly := range []bool{false, true} {
		name := "writable"
		if readOnly {
			name = "read-only"
		}
		t.Run(name, func(t *testing.T) {
			database := openDailyUsageFixtureDB(t)
			if readOnly {
				reader, err := OpenReadOnly(t.Context(), database.path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reader.Close()) })
				database = reader
			}
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
			assert.Equal(t, int32(3), prepares.Load())
		})
	}
}

func TestUsageWindowMemoKeepsIntegerFiltersDistinct(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 10)
	_, err := database.getWriter().Exec(t.Context(), `UPDATE sessions SET user_message_count = 55296 WHERE id = 'reported'`)
	require.NoError(t, err)
	ctx := WithUsageWindowMemo(t.Context())
	for _, minimum := range []int{55296, 55297} {
		filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", MinUserMessages: minimum}
		want := getDailyUsageLegacyForRollupTest(t, database, filter)
		got, err := database.GetDailyUsage(ctx, filter)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

func TestConcurrentUsageReadsSeparateArchiveStates(t *testing.T) {
	for _, change := range []string{"pricing", "usage replacement", "usage deletion", "cursor", "session project"} {
		t.Run(change, func(t *testing.T) {
			database := testDB(t)
			insertSession(t, database, "reported", "project", func(s *Session) {
				s.StartedAt = Ptr("2026-08-10T09:00:00Z")
			})
			events := []UsageEvent{{Source: "session", Model: "model-a", InputTokens: 10, OccurredAt: "2026-08-10T09:00:00Z", DedupKey: "event"}}
			require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), "reported", events))
			require.NoError(t, database.SetPricingMetaContext(t.Context(), "_openrouter_models", `["model-a"]`))
			require.NoError(t, database.UpsertModelPricingContext(t.Context(), []ModelPricing{{ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("1"), UpdatedAt: "2026-08-10T12:00:00Z"}}))
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var prepares atomic.Int32
			database.usageWindowObserver.beforePrepare = func(ctx context.Context) {
				if prepares.Add(1) == 1 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
			}
			filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
			type result struct {
				daily DailyUsageResult
				err   error
			}
			done := make(chan result, 2)
			read := func() {
				daily, err := database.GetDailyUsage(t.Context(), filter)
				done <- result{daily, err}
			}
			go read()
			<-entered
			applyUsageArchiveChange(t, database, change)
			go read()
			assert.Eventually(t, func() bool { return prepares.Load() == 2 }, 10*time.Second, time.Millisecond)
			unblock()
			want := getDailyUsageLegacyForRollupTest(t, database, filter)
			for range 2 {
				got := <-done
				require.NoError(t, got.err)
				assert.Equal(t, want, got.daily)
			}
		})
	}
}

func TestTopUsageReadsLeaveCursorUnprepared(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 10)
	require.NoError(t, database.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{OccurredAt: "2026-08-10T09:00:00Z", Model: "cursor-model", InputTokens: 7, DedupKey: "cursor"}}))
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	require.Positive(t, snapshot.CursorHighWater)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	ctx := WithUsageWindowMemo(t.Context())
	want, err := database.getTopSessionsByCostLegacy(t.Context(), filter, 3)
	require.NoError(t, err)
	got, err := database.GetTopSessionsByCost(ctx, filter, 3)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Zero(t, usageCacheCount(t, cache, "cursor_usage_facts"))
	var installs int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM usage_rollup_installs WHERE session_id = ?`, usageRollupCursorSessionID).Scan(&installs))
	assert.Zero(t, installs)
	daily, err := database.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, 17, daily.Totals.InputTokens)
	assert.Equal(t, 1, usageCacheCount(t, cache, "cursor_usage_facts"))
}

func TestConcurrentUsageReadsShareOneWindow(t *testing.T) {
	database := openDailyUsageFixtureDB(t)
	filters := []UsageFilter{
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC", Model: "model-b"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC", Model: "model-a"},
		{From: "2024-06-01", To: "2024-07-31", Timezone: "UTC", ExcludeModel: "model-b"},
	}
	wants := make([]DailyUsageResult, len(filters))
	for i, filter := range filters {
		wants[i] = getDailyUsageLegacyForRollupTest(t, database, filter)
	}
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
	key := usageFlightKeyForTest(t, database, filters[0])
	require.Eventually(t, func() bool { return database.usageWindows.Waiters(key) == 4 }, 10*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, (<-done[0]).err, context.Canceled)
	unblock()
	for i := 1; i < len(done); i++ {
		got := <-done[i]
		require.NoError(t, got.err)
		assert.Equal(t, wants[i], got.daily)
	}
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
	key := usageFlightKeyForTest(t, database, filter)
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

func usageFlightKeyForTest(t *testing.T, database *DB, filter UsageFilter) string {
	t.Helper()
	return database.usageFlightKey(usageWindowKey(filter, usageQueryKindToken, usageCursorIncluded(filter)))
}

func TestConcurrentUsageReadsSeeCommitsAfterWriterHandoff(t *testing.T) {
	for _, mode := range []string{"reopen", "reopen writer", "read-only", "closed writer"} {
		t.Run(mode, func(t *testing.T) {
			writer := testDB(t)
			seedLiveWriteSession(t, writer, "first", "project", "2026-08-10T09:00:00Z", 0, 10)
			database := writer
			before := database.archiveCommits.Load()
			switch mode {
			case "reopen":
				require.NoError(t, writer.Reopen())
				assert.Greater(t, database.archiveCommits.Load(), before+1, "reopen must invalidate sharing beyond the trigger-install commit")
			case "reopen writer":
				require.NoError(t, writer.CloseWriter())
				assert.Equal(t, before+1, database.archiveCommits.Load(), "closing the writer must invalidate sharing")
				before = database.archiveCommits.Load()
				require.NoError(t, writer.ReopenWriter())
				assert.Greater(t, database.archiveCommits.Load(), before+1, "reopening the writer must invalidate sharing beyond the trigger-install commit")
			case "read-only":
				reader, err := OpenReadOnly(t.Context(), writer.path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reader.Close()) })
				database = reader
			case "closed writer":
				require.NoError(t, database.CloseWriter())
				assert.Equal(t, before+1, database.archiveCommits.Load(), "closing the writer must invalidate sharing")
				external, err := OpenPreparedTestDB(database.path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, external.Close()) })
				writer = external
			}
			filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
			require.Equal(t, 10, getDailyUsageLegacyForRollupTest(t, database, filter).Totals.InputTokens)
			snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
			require.NoError(t, err)
			cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
			require.NoError(t, err)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var prepares, ensures atomic.Int32
			database.usageWindowObserver.beforePrepare = func(context.Context) { prepares.Add(1) }
			// Hold the first preparation after its archive snapshot was captured.
			cache.rollup.observer.beforeEnsure = func() {
				if ensures.Add(1) == 1 {
					close(entered)
					<-release
				}
			}
			type result struct {
				daily DailyUsageResult
				err   error
			}
			first, second := make(chan result, 1), make(chan result, 1)
			read := func(done chan<- result) {
				daily, err := database.GetDailyUsage(t.Context(), filter)
				done <- result{daily, err}
			}
			go read(first)
			<-entered
			seedLiveWriteSession(t, writer, "second", "project", "2026-08-10T09:00:00Z", 0, 7)
			go read(second)
			assert.Eventually(t, func() bool { return prepares.Load() == 2 }, 10*time.Second, time.Millisecond)
			unblock()
			old, fresh := <-first, <-second
			require.NoError(t, old.err)
			require.NoError(t, fresh.err)
			assert.Equal(t, 10, old.daily.Totals.InputTokens)
			assert.Equal(t, 17, fresh.daily.Totals.InputTokens)
			assert.Equal(t, getDailyUsageLegacyForRollupTest(t, writer, filter), fresh.daily)
		})
	}
}

func applyUsageArchiveChange(t *testing.T, database *DB, change string) {
	t.Helper()
	switch change {
	case "pricing":
		require.NoError(t, database.UpsertModelPricingContext(t.Context(), []ModelPricing{{ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("2"), UpdatedAt: "2026-08-10T12:00:00Z"}}))
	case "usage replacement":
		require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), "reported", []UsageEvent{{Source: "session", Model: "model-a", InputTokens: 17, OccurredAt: "2026-08-10T09:00:00Z", DedupKey: "event"}}))
	case "usage deletion":
		require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), "reported", nil))
	case "cursor":
		require.NoError(t, database.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{OccurredAt: "2026-08-10T09:00:00Z", Model: "cursor-model", InputTokens: 7, DedupKey: "cursor"}}))
	case "session project":
		_, err := database.getWriter().Exec(t.Context(), `UPDATE sessions SET project = 'project-b' WHERE id = 'reported'`)
		require.NoError(t, err)
	}
}

func TestArchiveCommitCountAdvancesOnCommitOnly(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "reported", "project", "2026-08-10T09:00:00Z", 0, 10)
	for _, change := range []string{"pricing", "usage replacement", "usage deletion", "cursor", "session project"} {
		t.Run(change, func(t *testing.T) {
			before := database.archiveCommits.Load()
			applyUsageArchiveChange(t, database, change)
			assert.Greater(t, database.archiveCommits.Load(), before)
		})
	}
	before := database.archiveCommits.Load()
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	want := getDailyUsageLegacyForRollupTest(t, database, filter)
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, before, database.archiveCommits.Load(), "reads and cache fills must preserve sharing")
	tx, err := database.getWriter().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE sessions SET project = 'rolled-back' WHERE id = 'reported'`)
	require.NoError(t, err)
	assert.Equal(t, before, database.archiveCommits.Load())
	require.NoError(t, tx.Rollback())
	assert.Equal(t, before, database.archiveCommits.Load())
	require.NoError(t, database.Reopen())
	assert.GreaterOrEqual(t, database.archiveCommits.Load(), before)
	before = database.archiveCommits.Load()
	applyUsageArchiveChange(t, database, "session project")
	assert.Greater(t, database.archiveCommits.Load(), before, "the reopened writer must retain commit tracking")
	require.NoError(t, database.CloseWriter())
	require.NoError(t, database.ReopenWriter())
	before = database.archiveCommits.Load()
	applyUsageArchiveChange(t, database, "session project")
	assert.Greater(t, database.archiveCommits.Load(), before, "writer handoff must retain commit tracking")
}
