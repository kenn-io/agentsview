//go:build fts5

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageCacheBackfillNewestFirstAndResumesInstalledCoverage(t *testing.T) {
	database := testDB(t)
	for _, fixture := range []struct {
		id, timestamp string
	}{
		{"old", "2026-08-01T10:00:00Z"},
		{"new", "2026-08-10T10:00:00Z"},
		{"middle", "2026-08-05T10:00:00Z"},
	} {
		insertSession(t, database, fixture.id, "project", func(session *Session) {
			session.StartedAt = &fixture.timestamp
			session.EndedAt = &fixture.timestamp
		})
		require.NoError(t, database.InsertMessages([]Message{{
			SessionID: fixture.id, Ordinal: 0, Role: "assistant",
			Timestamp: fixture.timestamp, Model: "model",
			TokenUsage: json.RawMessage(`{"input_tokens":1}`),
		}}))
	}

	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	activity, err := database.usageBackfillActivity(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "2026-08-10T10:00:00Z", activity["new"])
	assert.Equal(t, "2026-08-05T10:00:00Z", activity["middle"])
	var extracted [][]string
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) {
		ids := make([]string, len(versions))
		for index, version := range versions {
			ids[index] = version.SessionID
		}
		extracted = append(extracted, ids)
	}

	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	require.Equal(t, [][]string{{"new", "middle", "old"}}, extracted)
	assert.Equal(t, 3, usageCacheCount(t, cache, "usage_cached_sessions"))
	assert.Equal(t, 3, usageCacheCount(t, cache, "usage_rollup_installs"))
	assert.Equal(t, 3, usageCacheCount(t, cache, "usage_daily_rollups"))

	extracted = nil
	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	assert.Empty(t, extracted, "installed versions are coverage truth on restart")
}

func TestUsageCacheBackfillRecapturesChangedSource(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "moving-backfill", "project", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "moving-backfill", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))

	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	var mutations atomic.Int32
	var mutationErr atomic.Value
	cache.fill.observer.afterExtract = func([]usageSourceVersion) {
		if mutations.Add(1) != 1 {
			return
		}
		_, updateErr := database.getWriter().Exec(`
			UPDATE messages SET token_usage = '{"input_tokens":9}'
			WHERE session_id = 'moving-backfill';
			UPDATE sessions SET transcript_revision = 'later'
			WHERE id = 'moving-backfill'`)
		if updateErr != nil {
			mutationErr.Store(updateErr)
		}
	}

	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(t, stored.(error))
	}
	require.Positive(t, mutations.Load())

	daily, err := database.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", SkipSessionCounts: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 9, daily.Totals.InputTokens)
}

func TestUsageCacheBackfillPublishesStableCoverageWhileNewestSessionChanges(
	t *testing.T,
) {
	database := testDB(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	newestID := fmt.Sprintf("moving-batch-%03d", usageCacheBackfillBatchSize)
	messages := make([]Message, 0, usageCacheBackfillBatchSize+1)
	for i := range usageCacheBackfillBatchSize + 1 {
		id := fmt.Sprintf("moving-batch-%03d", i)
		timestamp := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		insertSession(t, database, id, "project", func(session *Session) {
			session.StartedAt = &timestamp
			session.EndedAt = &timestamp
		})
		messages = append(messages, Message{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Timestamp: timestamp, Model: "model",
			TokenUsage: json.RawMessage(`{"input_tokens":1}`),
		})
	}
	require.NoError(t, database.InsertMessages(messages))

	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	var mutations atomic.Int32
	var mutationErr atomic.Value
	cache.rollup.observer.beforeEnsure = func() {
		mutation := mutations.Add(1)
		usage := fmt.Sprintf(`{"input_tokens":%d}`, 1+mutation)
		_, updateErr := database.getWriter().Exec(
			`UPDATE messages SET token_usage = ? WHERE session_id = ?`,
			usage, newestID,
		)
		if updateErr == nil {
			_, updateErr = database.getWriter().Exec(
				`UPDATE sessions SET transcript_revision = ? WHERE id = ?`,
				fmt.Sprintf("moving-%d", mutation), newestID,
			)
		}
		if updateErr != nil {
			mutationErr.Store(updateErr)
		}
	}

	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	cache.rollup.observer = usageRollupObserver{}
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(t, stored.(error))
	}
	require.Positive(t, mutations.Load())

	metadata := readUsageCacheMetadata(t, cache.db)
	assert.NotEmpty(t, metadata[usageCacheMetadataBackfillCompletedAt])
	daily, err := database.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-08-01", To: "2026-08-01", Timezone: "UTC",
		SkipSessionCounts: true,
	})
	require.NoError(t, err)
	assert.Equal(t,
		usageCacheBackfillBatchSize+int(mutations.Load())+1,
		daily.Totals.InputTokens,
	)
}

func TestUsageCacheBackfillBatchesNewestSessionsFirst(t *testing.T) {
	database := testDB(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	messages := make([]Message, 0, usageCacheBackfillBatchSize+1)
	for i := range usageCacheBackfillBatchSize + 1 {
		id := fmt.Sprintf("batch-%03d", i)
		timestamp := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		insertSession(t, database, id, "project", func(session *Session) {
			session.StartedAt = &timestamp
			session.EndedAt = &timestamp
		})
		messages = append(messages, Message{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Timestamp: timestamp, Model: "model",
			TokenUsage: json.RawMessage(`{"input_tokens":1}`),
		})
	}
	require.NoError(t, database.InsertMessages(messages))

	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	var extracted [][]string
	maintenanceCalls := 0
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) {
		ids := make([]string, len(versions))
		for index, version := range versions {
			ids[index] = version.SessionID
		}
		extracted = append(extracted, ids)
	}
	cache.fill.observer.afterMaintenance = func() { maintenanceCalls++ }

	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	require.Len(t, extracted, 2)
	assert.Equal(t, 1, maintenanceCalls,
		"the outer backfill must maintain the cache between batches")
	assert.Equal(t, 1, usageCacheCount(t, cache, "usage_rollup_timezones"),
		"one backfill pass must use one process-local timezone generation")
	assert.Len(t, extracted[0], usageCacheBackfillBatchSize)
	assert.Equal(t, fmt.Sprintf("batch-%03d", usageCacheBackfillBatchSize),
		extracted[0][0])
	assert.Equal(t, "batch-001", extracted[0][usageCacheBackfillBatchSize-1])
	assert.Equal(t, []string{"batch-000"}, extracted[1])
}

func TestUsageCacheBackfillRewarmsEightRecentExplicitTimezones(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T10:00:00Z"
	insertSession(t, database, "recent-zone", "project", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "recent-zone", Ordinal: 0, Role: "assistant",
		Timestamp: started, Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	oldestKey := ""
	for index := 1; index <= 9; index++ {
		name := fmt.Sprintf("Etc/GMT+%d", index)
		location, err := time.LoadLocation(name)
		require.NoError(t, err)
		identity := usageTimezoneIdentityFor(location, nil)
		if index == 1 {
			oldestKey = identity.Key
		}
		_, err = cache.db.Exec(`INSERT INTO usage_rollup_timezones(
			timezone_key, timezone_name, interval_fingerprint, last_requested_at
		) VALUES (?, ?, ?, ?)`, identity.Key, identity.Name,
			identity.IntervalFingerprint,
			fmt.Sprintf("2026-08-%02dT00:00:00Z", index))
		require.NoError(t, err)
	}

	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))

	assert.Equal(t, 10, usageCacheCount(t, cache, "usage_rollup_timezones"))
	assert.Equal(t, 9, usageCacheCount(t, cache, "usage_rollup_installs"),
		"the local zone plus eight explicit zones should be warmed")
	var oldestInstalls int
	require.NoError(t, cache.db.QueryRow(`SELECT COUNT(*)
		FROM usage_rollup_installs i JOIN usage_rollup_timezones tz
		  ON tz.id = i.timezone_id
		WHERE tz.timezone_key = ?`, oldestKey).Scan(&oldestInstalls))
	assert.Zero(t, oldestInstalls, "the ninth explicit zone should not be rewarmed")
}

func TestUsageCacheBackfillObserverAttachesToActivePass(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "active", "project")
	started := make(chan struct{})
	release := make(chan struct{})
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	cache.fill.observer.beforeExtract = func([]usageSourceVersion) {
		close(started)
		<-release
	}
	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	<-started
	observed := make(chan struct{}, 1)
	database.SetUsageCacheBackfillStarted(func() { observed <- struct{}{} })
	select {
	case <-observed:
	case <-time.After(30 * time.Second):
		t.Fatal("observer did not attach to the active backfill")
	}
	close(release)
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
}

func TestUsageCacheBackfillSweepsHardDeletionTombstones(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "deleted", "project", func(session *Session) {
		session.StartedAt = Ptr("2026-08-10T10:00:00Z")
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "deleted", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T10:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))

	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	assert.Equal(t, 1, usageCacheCount(t, cache, "usage_cached_sessions"))
	require.NoError(t, database.DeleteSession("deleted"))
	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	assert.Zero(t, usageCacheCount(t, cache, "usage_cached_sessions"))
	assert.Zero(t, usageCacheCount(t, cache, "usage_rollup_installs"),
		"deletion hygiene must reclaim every timezone rollup for the session")
}

func TestUsageMatchingCountDiscoversWritesAfterCompletedBackfill(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	_, err := database.getWriter().Exec(`
		INSERT INTO sessions(id, project, machine, agent, started_at)
		VALUES ('external-activity', 'project', 'machine', 'claude',
		        '2026-08-12T10:00:00Z');
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, model)
		VALUES ('external-activity', 0, 'assistant', 'answer',
		        '2026-08-12T10:01:00Z', 'model')`)
	require.NoError(t, err)
	count, err := database.GetUsageMatchingSessionCount(
		context.Background(), UsageFilter{
			From: "2026-08-12", To: "2026-08-12", Timezone: "UTC",
		})
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestUsageCacheIncrementalVacuumThreshold(t *testing.T) {
	database := testDB(t)
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)

	ran, err := cache.incrementalVacuum(context.Background(), 4096, 256)
	require.NoError(t, err)
	assert.False(t, ran)
	_, err = cache.db.Exec(`CREATE TABLE vacuum_fixture(value BLOB)`)
	require.NoError(t, err)
	for range 4200 {
		_, err = cache.db.Exec(
			`INSERT INTO vacuum_fixture VALUES (zeroblob(4096))`)
		require.NoError(t, err)
	}
	_, err = cache.db.Exec(`DELETE FROM vacuum_fixture`)
	require.NoError(t, err)
	ran, err = cache.incrementalVacuum(context.Background(), 4096, 256)
	require.NoError(t, err)
	assert.True(t, ran)
}

func TestUsageCacheBackfillStopJoinsWorker(t *testing.T) {
	database := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, database.StartUsageCacheBackfill(ctx))
	database.StopUsageCacheBackfill()
	done := make(chan struct{})
	go func() {
		_ = database.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not join usage cache backfill")
	}
}

func TestCloseConnectionsStopsUsageCacheBackfill(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	cache.fill.observer.beforeExtract = func([]usageSourceVersion) {
		close(started)
		<-release
	}
	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	<-started
	closed := make(chan error, 1)
	go func() { closed <- database.CloseConnections() }()
	var earlyErr error
	returnedEarly := false
	select {
	case earlyErr = <-closed:
		returnedEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if returnedEarly {
		require.NoError(t, earlyErr)
		t.Fatal("CloseConnections returned before backfill stopped")
	}
	select {
	case err = <-closed:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("CloseConnections did not join cancelled usage cache backfill")
	}
	cache.fill.observer = usageFillObserver{}
	require.NoError(t, database.Reopen())
}

func TestUsageCacheBackfillRebuildsRollupsInvalidatedByLaterBatches(t *testing.T) {
	database := testDB(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	messages := make([]Message, 0, usageCacheBackfillBatchSize+1)
	newest := fmt.Sprintf("batch-%03d", usageCacheBackfillBatchSize)
	for i := range usageCacheBackfillBatchSize + 1 {
		id := fmt.Sprintf("batch-%03d", i)
		timestamp := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		insertSession(t, database, id, "project", func(session *Session) {
			session.StartedAt = &timestamp
			session.EndedAt = &timestamp
		})
		message := Message{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Timestamp: timestamp, Model: "model",
			TokenUsage: json.RawMessage(`{"input_tokens":1}`),
		}
		// The newest session (first batch) and the oldest session (last
		// batch) share one snapshot identity: filling the oldest batch
		// invalidates the rollup already installed for the newest.
		if i == 0 || id == newest {
			message.ClaudeMessageID = "shared-batch-message"
			message.ClaudeRequestID = "shared-batch-request"
		}
		messages = append(messages, message)
	}
	require.NoError(t, database.InsertMessages(messages))

	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)

	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))

	assert.Equal(t, usageCacheBackfillBatchSize+1,
		usageCacheCount(t, cache, "usage_rollup_installs"),
		"backfill must rebuild rollups its later batches invalidated")
	var missing int
	require.NoError(t, cache.db.QueryRow(`SELECT count(*)
		FROM usage_cached_sessions c
		WHERE NOT EXISTS (
			SELECT 1 FROM usage_rollup_installs i
			WHERE i.session_id = c.session_id
		)`).Scan(&missing))
	assert.Zero(t, missing, "every cached session must keep a rollup install")
}

func TestReopenRestartsUsageBackfillOnlyWhenPreviouslyStarted(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.Reopen())
	database.usageBackfillMu.Lock()
	done := database.usageBackfillDone
	database.usageBackfillMu.Unlock()
	require.Nil(t, done,
		"reopen must not start a backfill pass nothing enabled")

	require.NoError(t, database.StartUsageCacheBackfill(context.Background()))
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
	require.NoError(t, database.Reopen())
	database.usageBackfillMu.Lock()
	done = database.usageBackfillDone
	database.usageBackfillMu.Unlock()
	require.NotNil(t, done,
		"reopen must restart backfill once it was explicitly started")
	require.NoError(t, database.WaitUsageCacheBackfill(context.Background()))
}
