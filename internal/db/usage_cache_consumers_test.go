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
	for _, readOnly := range []bool{false, true} {
		name := "writable"
		if readOnly {
			name = "read-only"
		}
		t.Run(name, func(t *testing.T) {
			database := openDailyUsageFixtureDB(t)
			seedLiveWriteSession(t, database, "reported", "project", "2024-06-10T09:00:00Z", 0, 10)
			_, err := database.getWriter().Exec(t.Context(), `UPDATE sessions SET user_message_count = 55296 WHERE id = 'reported'`)
			require.NoError(t, err)
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
			assert.Equal(t, int32(2), prepares.Load())
			for _, minimum := range []int{55296, 55297} {
				filter := UsageFilter{From: "2024-06-01", To: "2024-07-31", Timezone: "America/Chicago", MinUserMessages: minimum}
				want := getDailyUsageLegacyForRollupTest(t, database, filter)
				got, err := database.GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				assert.Equal(t, want, got)
			}
			assert.Equal(t, int32(4), prepares.Load())
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
	for i, filter := range filters {
		done[i] = make(chan dailyResult, 1)
		go func() {
			daily, err := database.GetDailyUsage(t.Context(), filter)
			done[i] <- dailyResult{daily, err}
		}()
	}
	key := usageFlightKeyForTest(t, database, filters[0])
	require.Eventually(t, func() bool { return database.usageWindows.Waiters(key) == 4 }, 10*time.Second, time.Millisecond)
	unblock()
	for i := range done {
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
	version, err := database.usageWindowVersion(t.Context())
	require.NoError(t, err)
	return usageWindowKey(filter, usageQueryKindToken, usageCursorIncluded(filter)) + "|" + version
}

func TestConcurrentUsageReadsSeeCommitsAfterWriterHandoff(t *testing.T) {
	for _, mode := range []string{"commit", "reopen", "read-only"} {
		t.Run(mode, func(t *testing.T) {
			writer := testDB(t)
			seedLiveWriteSession(t, writer, "first", "project", "2026-08-10T09:00:00Z", 0, 10)
			database := writer
			switch mode {
			case "reopen":
				require.NoError(t, writer.Reopen())
			case "read-only":
				reader, err := OpenReadOnly(t.Context(), writer.path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reader.Close()) })
				database = reader
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

func TestPostCommitRequestSeesPublishedMetadata(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "session", "before", "2026-08-10T09:00:00Z", 0, 10)
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	_, err := database.GetTopSessionsByCost(t.Context(), filter, 3)
	require.NoError(t, err)
	snapshot, err := database.captureUsageQuery(t.Context(), filter, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs`)
	require.NoError(t, err)
	// Keep the transcript in SQLite's page cache until COMMIT to exercise
	// the interval between the commit hook and WAL publication.
	_, err = database.getWriter().Exec(t.Context(), `PRAGMA cache_size = -524288`)
	require.NoError(t, err)
	_, err = database.getWriter().Exec(t.Context(), `PRAGMA cache_spill = OFF`)
	require.NoError(t, err)
	tx, err := database.getWriter().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), `UPDATE messages SET content = hex(zeroblob(128*1024*1024)) WHERE session_id = 'session'`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE sessions SET project = 'after' WHERE id = 'session'`)
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var prepares atomic.Int32
	database.usageWindowObserver.beforePrepare = func(context.Context) { prepares.Add(1) }
	var enteredOnce sync.Once
	cache.rollup.observer.beforeEnsure = func() {
		enteredOnce.Do(func() { close(entered) })
		<-release
	}
	committed := make(chan error, 1)
	go func() { committed <- tx.Commit() }()
	select {
	case err := <-committed:
		require.NoError(t, err)
		require.FailNow(t, "commit published before the old snapshot could be captured")
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: assert the large commit stays unpublished during the capture delay.
	}
	type result struct {
		entries []TopSessionEntry
		err     error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	read := func(done chan<- result) {
		entries, err := database.GetTopSessionsByCost(t.Context(), filter, 3)
		done <- result{entries, err}
	}
	go read(first)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "preparation never reached rollup")
	}
	require.NoError(t, <-committed)
	select {
	case <-first:
		require.FailNow(t, "leader finished before release")
	default:
	}
	version, err := database.usageWindowVersion(t.Context())
	require.NoError(t, err)
	key := usageWindowKey(filter, usageQueryKindToken, false) + "|" + version
	go read(second)
	assert.Eventually(t, func() bool { return prepares.Load() == 2 }, 10*time.Second, time.Millisecond)
	assert.Equal(t, 1, database.usageWindows.Waiters(key))
	unblock()
	old, fresh := <-first, <-second
	require.NoError(t, old.err)
	require.NoError(t, fresh.err)
	require.Len(t, old.entries, 1)
	assert.Equal(t, "before", old.entries[0].Project)
	require.Len(t, fresh.entries, 1)
	assert.Equal(t, "after", fresh.entries[0].Project)
}

func TestConcurrentUsageReadsSeeOtherHandleCommits(t *testing.T) {
	database := testDB(t)
	seedLiveWriteSession(t, database, "session", "before", "2026-08-10T09:00:00Z", 0, 10)
	writer, err := Open(t.Context(), database.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
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
	cache.rollup.observer.beforeEnsure = func() {
		if ensures.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	type result struct {
		entries []TopSessionEntry
		err     error
	}
	first, second := make(chan result, 1), make(chan result, 1)
	read := func(done chan<- result) {
		entries, err := database.GetTopSessionsByCost(t.Context(), filter, 3)
		done <- result{entries, err}
	}
	go read(first)
	<-entered
	_, err = writer.getWriter().Exec(t.Context(), `UPDATE sessions SET project = 'after' WHERE id = 'session'`)
	require.NoError(t, err)
	go read(second)
	assert.Eventually(t, func() bool { return prepares.Load() == 2 }, 10*time.Second, time.Millisecond)
	unblock()
	old, fresh := <-first, <-second
	require.NoError(t, old.err)
	require.NoError(t, fresh.err)
	require.Len(t, old.entries, 1)
	assert.Equal(t, "before", old.entries[0].Project)
	require.Len(t, fresh.entries, 1)
	assert.Equal(t, "after", fresh.entries[0].Project)
}

func TestReadOnlyConcurrentSidesPrepareOnce(t *testing.T) {
	writer := openDailyUsageFixtureDB(t)
	database, err := OpenReadOnly(t.Context(), writer.path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
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
	defer unblock()
	var prepares atomic.Int32
	database.usageWindowObserver.beforePrepare = func(context.Context) {
		if prepares.Add(1) == 1 {
			close(entered)
		}
		<-release
	}
	ctx := WithUsageWindowMemo(t.Context())
	type result struct {
		model string
		daily DailyUsageResult
		err   error
	}
	done := make(chan result, 2)
	for _, model := range []string{"model-a", "model-b"} {
		go func() {
			side := filter
			side.Model = model
			daily, err := database.GetDailyUsage(ctx, side)
			done <- result{model, daily, err}
		}()
	}
	<-entered
	key := usageFlightKeyForTest(t, database, filter)
	assert.Eventually(t, func() bool { return database.usageWindows.Waiters(key) == 2 || prepares.Load() == 2 }, 10*time.Second, time.Millisecond)
	assert.Equal(t, int32(1), prepares.Load())
	unblock()
	for range 2 {
		got := <-done
		require.NoError(t, got.err)
		assert.Equal(t, wants[got.model], got.daily)
	}
}
