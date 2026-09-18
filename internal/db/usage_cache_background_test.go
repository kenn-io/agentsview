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
	assert := assert.New(t)
	require := require.New(t)

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
		require.NoError(database.InsertMessages([]Message{{
			SessionID: fixture.id, Ordinal: 0, Role: "assistant",
			Timestamp: fixture.timestamp, Model: "model",
			TokenUsage: json.RawMessage(`{"input_tokens":1}`),
		}}))
	}

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	activity, err := database.usageBackfillActivity(t.Context())
	require.NoError(err)
	assert.Equal("2026-08-10T10:00:00Z", activity["new"])
	assert.Equal("2026-08-05T10:00:00Z", activity["middle"])
	var extracted [][]string
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) {
		ids := make([]string, len(versions))
		for index, version := range versions {
			ids[index] = version.SessionID
		}
		extracted = append(extracted, ids)
	}

	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))
	require.Equal([][]string{{"new", "middle", "old"}}, extracted)
	assert.Equal(3, usageCacheCount(t, cache, "usage_cached_sessions"))
	assert.Equal(3, usageCacheCount(t, cache, "usage_rollup_installs"))
	assert.Equal(3, usageCacheCount(t, cache, "usage_daily_rollups"))

	extracted = nil
	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))
	assert.Empty(extracted, "installed versions are coverage truth on restart")
}

// The pass no longer recaptures when a source moves under it: it installs the
// facts its read snapshot saw, and the next request refills the session that
// changed. The observable outcome, an exact answer afterwards, is unchanged.
func TestUsageCacheBackfillPicksUpSourceChangedDuringPass(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "moving-backfill", "project", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(database.InsertMessages([]Message{{
		SessionID: "moving-backfill", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)
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

	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(stored.(error))
	}
	require.Positive(mutations.Load())

	daily, err := database.GetDailyUsage(t.Context(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", SkipSessionCounts: true,
	})
	require.NoError(err)
	assert.Equal(t, 9, daily.Totals.InputTokens)
}

func TestUsageCacheBackfillPublishesStableCoverageWhileNewestSessionChanges(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(database.InsertMessages(messages))

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)
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

	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))
	cache.rollup.observer = usageRollupObserver{}
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(stored.(error))
	}
	require.Positive(mutations.Load())

	metadata := readUsageCacheMetadata(t, cache.db)
	assert.NotEmpty(metadata[usageCacheMetadataBackfillCompletedAt])
	daily, err := database.GetDailyUsage(t.Context(), UsageFilter{
		From: "2026-08-01", To: "2026-08-01", Timezone: "UTC",
		SkipSessionCounts: true,
	})
	require.NoError(err)
	assert.Equal(usageCacheBackfillBatchSize+int(mutations.Load())+1,
		daily.Totals.InputTokens,
	)
}

func TestUsageCacheBackfillBatchesNewestSessionsFirst(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(database.InsertMessages(messages))

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)
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

	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))
	require.Len(extracted, 2)
	assert.Equal(1, maintenanceCalls,
		"the outer backfill must maintain the cache between batches")
	assert.Equal(1, usageCacheCount(t, cache, "usage_rollup_timezones"),
		"one backfill pass must use one process-local timezone generation")
	assert.Len(extracted[0], usageCacheBackfillBatchSize)
	assert.Equal(fmt.Sprintf("batch-%03d", usageCacheBackfillBatchSize),
		extracted[0][0])
	assert.Equal("batch-001", extracted[0][usageCacheBackfillBatchSize-1])
	assert.Equal([]string{"batch-000"}, extracted[1])
}

func TestUsageCacheBackfillRewarmsEightRecentExplicitTimezones(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	started := "2026-08-10T10:00:00Z"
	insertSession(t, database, "recent-zone", "project", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(database.InsertMessages([]Message{{
		SessionID: "recent-zone", Ordinal: 0, Role: "assistant",
		Timestamp: started, Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	oldestKey := ""
	for index := 1; index <= 9; index++ {
		name := fmt.Sprintf("Etc/GMT+%d", index)
		location, err := time.LoadLocation(name)
		require.NoError(err)
		identity := usageTimezoneIdentityFor(location, nil)
		if index == 1 {
			oldestKey = identity.Key
		}
		_, err = cache.db.ExecContext(t.Context(), `INSERT INTO usage_rollup_timezones(
			timezone_key, timezone_name, interval_fingerprint, last_requested_at
		) VALUES (?, ?, ?, ?)`, identity.Key, identity.Name,
			identity.IntervalFingerprint,
			fmt.Sprintf("2026-08-%02dT00:00:00Z", index))
		require.NoError(err)
	}

	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))

	assert.Equal(10, usageCacheCount(t, cache, "usage_rollup_timezones"))
	assert.Equal(9, usageCacheCount(t, cache, "usage_rollup_installs"),
		"the local zone plus eight explicit zones should be warmed")
	var oldestInstalls int
	require.NoError(cache.db.QueryRowContext(t.Context(), `SELECT COUNT(*)
		FROM usage_rollup_installs i JOIN usage_rollup_timezones tz
		  ON tz.id = i.timezone_id
		WHERE tz.timezone_key = ?`, oldestKey).Scan(&oldestInstalls))
	assert.Zero(oldestInstalls, "the ninth explicit zone should not be rewarmed")
}

func TestUsageCacheBackfillObserverAttachesToActivePass(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	insertSession(t, database, "active", "project")
	started := make(chan struct{})
	release := make(chan struct{})
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	cache.fill.observer.beforeExtract = func([]usageSourceVersion) {
		close(started)
		<-release
	}
	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	<-started
	observed := make(chan struct{}, 1)
	database.SetUsageCacheBackfillStarted(func() { observed <- struct{}{} })
	select {
	case <-observed:
	case <-time.After(30 * time.Second):
		t.Fatal("observer did not attach to the active backfill")
	}
	close(release)
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))
}

func TestUsageCacheBackfillSweepsHardDeletionTombstones(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	insertSession(t, database, "deleted", "project", func(session *Session) {
		session.StartedAt = Ptr("2026-08-10T10:00:00Z")
	})
	require.NoError(database.InsertMessages([]Message{{
		SessionID: "deleted", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T10:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	assert.Equal(1, usageCacheCount(t, cache, "usage_cached_sessions"))
	require.NoError(database.DeleteSession("deleted"))
	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))
	assert.Zero(usageCacheCount(t, cache, "usage_cached_sessions"))
	assert.Zero(usageCacheCount(t, cache, "usage_rollup_installs"),
		"deletion hygiene must reclaim every timezone rollup for the session")
}

func TestUsageCacheBackfillContinuesWhenSessionIsDeletedDuringRollup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	insertSession(t, database, "deleted-during-rollup", "project", func(session *Session) {
		session.StartedAt = Ptr("2026-08-10T10:00:00Z")
	})
	require.NoError(database.InsertMessages([]Message{{
		SessionID: "deleted-during-rollup", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T10:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindActivity)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	deleted := make(chan error, 1)
	var once atomic.Bool
	cache.rollup.observer.beforeInstall = func(builds []usageRollupBuild) {
		if len(builds) == 0 || !once.CompareAndSwap(false, true) {
			return
		}
		deleteErr := database.DeleteSession("deleted-during-rollup")
		if deleteErr == nil {
			deleteErr = cache.fill.deleteNotificationSessions(
				[]string{"deleted-during-rollup"})
		}
		deleted <- deleteErr
	}

	require.NoError(database.runUsageCacheBackfillPass(
		t.Context(), time.Now(), snapshot))
	require.NoError(<-deleted)
	assert.Zero(usageCacheCount(t, cache, "usage_cached_sessions"))
	assert.Zero(usageCacheCount(t, cache, "usage_rollup_installs"))
	assert.NotEmpty(readUsageCacheMetadata(t, cache.db)[usageCacheMetadataBackfillCompletedAt])
}

func TestUsageMatchingCountDiscoversWritesAfterCompletedBackfill(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))
	_, err := database.getWriter().Exec(`
		INSERT INTO sessions(id, project, machine, agent, started_at)
		VALUES ('external-activity', 'project', 'machine', 'claude',
		        '2026-08-12T10:00:00Z');
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, model)
		VALUES ('external-activity', 0, 'assistant', 'answer',
		        '2026-08-12T10:01:00Z', 'model')`)
	require.NoError(err)
	count, err := database.GetUsageMatchingSessionCount(
		t.Context(), UsageFilter{
			From: "2026-08-12", To: "2026-08-12", Timezone: "UTC",
		})
	require.NoError(err)
	assert.Equal(t, 1, count)
}

func TestUsageCacheIncrementalVacuumThreshold(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)

	ran, err := cache.incrementalVacuum(t.Context(), 4096, 256)
	require.NoError(err)
	assert.False(ran)
	_, err = cache.db.ExecContext(t.Context(), `CREATE TABLE vacuum_fixture(value BLOB)`)
	require.NoError(err)
	for range 4200 {
		_, err = cache.db.ExecContext(t.Context(),
			`INSERT INTO vacuum_fixture VALUES (zeroblob(4096))`)
		require.NoError(err)
	}
	_, err = cache.db.ExecContext(t.Context(), `DELETE FROM vacuum_fixture`)
	require.NoError(err)
	ran, err = cache.incrementalVacuum(t.Context(), 4096, 256)
	require.NoError(err)
	assert.True(ran)
}

func TestUsageCacheBackfillStopJoinsWorker(t *testing.T) {
	database := testDB(t)
	ctx, cancel := context.WithCancel(t.Context())
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
	require := require.New(t)

	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	started := make(chan struct{})
	release := make(chan struct{})
	cache.fill.observer.beforeExtract = func([]usageSourceVersion) {
		close(started)
		<-release
	}
	require.NoError(database.StartUsageCacheBackfill(t.Context()))
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
		require.NoError(earlyErr)
		t.Fatal("CloseConnections returned before backfill stopped")
	}
	select {
	case err = <-closed:
		require.NoError(err)
	case <-time.After(30 * time.Second):
		t.Fatal("CloseConnections did not join cancelled usage cache backfill")
	}
	cache.fill.observer = usageFillObserver{}
	require.NoError(database.Reopen())
}

func TestUsageCacheBackfillRebuildsRollupsInvalidatedByLaterBatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(database.InsertMessages(messages))

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(err)

	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))

	assert.Equal(usageCacheBackfillBatchSize+1,
		usageCacheCount(t, cache, "usage_rollup_installs"),
		"backfill must rebuild rollups its later batches invalidated")
	var missing int
	require.NoError(cache.db.QueryRowContext(t.Context(), `SELECT count(*)
		FROM usage_cached_sessions c
		WHERE NOT EXISTS (
			SELECT 1 FROM usage_rollup_installs i
			WHERE i.session_id = c.session_id
		)`).Scan(&missing))
	assert.Zero(missing, "every cached session must keep a rollup install")
}

func TestReopenRestartsUsageBackfillOnlyWhenPreviouslyStarted(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	require.NoError(database.Reopen())
	database.usageBackfillMu.Lock()
	done := database.usageBackfillDone
	database.usageBackfillMu.Unlock()
	require.Nil(done,
		"reopen must not start a backfill pass nothing enabled")

	require.NoError(database.StartUsageCacheBackfill(t.Context()))
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))
	require.NoError(database.Reopen())
	database.usageBackfillMu.Lock()
	done = database.usageBackfillDone
	database.usageBackfillMu.Unlock()
	require.NotNil(done,
		"reopen must restart backfill once it was explicitly started")
	require.NoError(database.WaitUsageCacheBackfill(t.Context()))
}
