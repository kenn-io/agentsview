//go:build fts5

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
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
		require.NoError(t, database.InsertMessages(t.Context(), []Message{{
			SessionID: fixture.id, Ordinal: 0, Role: "assistant",
			Timestamp: fixture.timestamp, Model: "model",
			TokenUsage: json.RawMessage(`{"input_tokens":1}`),
		}}))
	}

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	activity, err := database.usageBackfillActivity(t.Context())
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

	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	require.Equal(t, [][]string{{"new", "middle", "old"}}, extracted)
	assert.Equal(t, 3, usageCacheCount(t, cache, "usage_cached_sessions"))
	assert.Equal(t, 3, usageCacheCount(t, cache, "usage_rollup_installs"))
	assert.Equal(t, 3, usageCacheCount(t, cache, "usage_daily_rollups"))

	extracted = nil
	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	assert.Empty(t, extracted, "installed versions are coverage truth on restart")
}

// The pass no longer recaptures when a source moves under it: it installs the
// facts its read snapshot saw, and the next request refills the session that
// changed. The observable outcome, an exact answer afterwards, is unchanged.
func TestUsageCacheBackfillPicksUpSourceChangedDuringPass(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "moving-backfill", "project", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "moving-backfill", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	var mutations atomic.Int32
	var mutationErr atomic.Value
	cache.fill.observer.afterExtract = func([]usageSourceVersion) {
		if mutations.Add(1) != 1 {
			return
		}
		_, updateErr := database.getWriter().Exec(t.Context(), `
			UPDATE messages SET token_usage = '{"input_tokens":9}'
			WHERE session_id = 'moving-backfill';
			UPDATE sessions SET transcript_revision = 'later'
			WHERE id = 'moving-backfill'`)
		if updateErr != nil {
			mutationErr.Store(updateErr)
		}
	}

	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(t, stored.(error))
	}
	require.Positive(t, mutations.Load())

	daily, err := database.GetDailyUsage(t.Context(), UsageFilter{
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
	require.NoError(t, database.InsertMessages(t.Context(), messages))

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	var mutations atomic.Int32
	var mutationErr atomic.Value
	cache.rollup.observer.beforeEnsure = func() {
		mutation := mutations.Add(1)
		usage := fmt.Sprintf(`{"input_tokens":%d}`, 1+mutation)
		_, updateErr := database.getWriter().Exec(t.Context(),
			`UPDATE messages SET token_usage = ? WHERE session_id = ?`,
			usage, newestID,
		)
		if updateErr == nil {
			_, updateErr = database.getWriter().Exec(t.Context(),
				`UPDATE sessions SET transcript_revision = ? WHERE id = ?`,
				fmt.Sprintf("moving-%d", mutation), newestID,
			)
		}
		if updateErr != nil {
			mutationErr.Store(updateErr)
		}
	}

	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	cache.rollup.observer = usageRollupObserver{}
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(t, stored.(error))
	}
	require.Positive(t, mutations.Load())

	metadata := readUsageCacheMetadata(t, cache.db)
	assert.NotEmpty(t, metadata[usageCacheMetadataBackfillCompletedAt])
	daily, err := database.GetDailyUsage(t.Context(), UsageFilter{
		From: "2026-08-01", To: "2026-08-01", Timezone: "UTC",
		SkipSessionCounts: true,
	})
	require.NoError(t, err)
	assert.Equal(t, usageCacheBackfillBatchSize+int(mutations.Load())+1,
		daily.Totals.InputTokens,
	)
}

func seedUsageBatchSessions(t *testing.T, database *DB, count int, base time.Time) {
	t.Helper()
	messages := make([]Message, 0, count)
	for i := range count {
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
	require.NoError(t, database.InsertMessages(t.Context(), messages))
}

func TestUsageCacheBackfillBatchesNewestSessionsFirst(t *testing.T) {
	database := testDB(t)
	seedUsageBatchSessions(t, database, usageCacheBackfillBatchSize+1,
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
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

	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
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
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "recent-zone", Ordinal: 0, Role: "assistant",
		Timestamp: started, Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
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
		_, err = cache.db.ExecContext(t.Context(), `INSERT INTO usage_rollup_timezones(
			timezone_key, timezone_name, interval_fingerprint, last_requested_at
		) VALUES (?, ?, ?, ?)`, identity.Key, identity.Name,
			identity.IntervalFingerprint,
			fmt.Sprintf("2026-08-%02dT00:00:00Z", index))
		require.NoError(t, err)
	}

	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))

	assert.Equal(t, 10, usageCacheCount(t, cache, "usage_rollup_timezones"))
	assert.Equal(t, 9, usageCacheCount(t, cache, "usage_rollup_installs"),
		"the local zone plus eight explicit zones should be warmed")
	var oldestInstalls int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT COUNT(*)
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
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	cache.fill.observer.beforeExtract = func([]usageSourceVersion) {
		close(started)
		<-release
	}
	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	<-started
	observed := make(chan struct{}, 1)
	database.SetUsageCacheBackfillStarted(func() { observed <- struct{}{} })
	select {
	case <-observed:
	case <-time.After(30 * time.Second):
		require.Fail(t, "observer did not attach to the active backfill")
	}
	close(release)
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
}

func TestUsageCacheBackfillRewarmsZoneRequestedDuringPass(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "active-zone", "project")
	snapshot, err := database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	held, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var once sync.Once
	cache.fill.observer.beforeExtract = func([]usageSourceVersion) {
		once.Do(func() { close(held); <-release })
	}
	t.Cleanup(func() { unblock(); database.StopUsageCacheBackfill() })
	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	<-held

	location, err := time.LoadLocation("Etc/GMT+3")
	require.NoError(t, err)
	identity := usageTimezoneIdentityFor(location, nil)
	_, err = cache.db.ExecContext(t.Context(), `INSERT INTO usage_rollup_timezones
		(timezone_key, timezone_name, interval_fingerprint, last_requested_at)
		VALUES (?, ?, ?, ?)`, identity.Key, identity.Name, identity.IntervalFingerprint, time.Now().UTC().Format(time.RFC3339Nano))
	require.NoError(t, err)
	for range 10 {
		_, _, err := database.prepareJournaledUsageWindow(t.Context(), UsageFilter{Timezone: identity.Name}, usageQueryKindToken, false, nil)
		require.NoError(t, err)
	}
	unblock()
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))

	zone, err := readUsageZoneCoverage(t.Context(), cache.db, identity.Key)
	require.NoError(t, err)
	assert.True(t, zone.revision.Valid, "the queued pass must claim the requested timezone")
}

func TestUsageCacheBackfillSweepsHardDeletionTombstones(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "deleted", "project", func(session *Session) {
		session.StartedAt = Ptr("2026-08-10T10:00:00Z")
	})
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "deleted", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T10:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	assert.Equal(t, 1, usageCacheCount(t, cache, "usage_cached_sessions"))
	require.NoError(t, database.DeleteSession(t.Context(), "deleted"))
	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	assert.Zero(t, usageCacheCount(t, cache, "usage_cached_sessions"))
	assert.Zero(t, usageCacheCount(t, cache, "usage_rollup_installs"),
		"deletion hygiene must reclaim every timezone rollup for the session")
}

func TestUsageCacheBackfillContinuesWhenSessionIsDeletedDuringRollup(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "deleted-during-rollup", "project", func(session *Session) {
		session.StartedAt = Ptr("2026-08-10T10:00:00Z")
	})
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "deleted-during-rollup", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T10:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindActivity)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	deleted := make(chan error, 1)
	var once atomic.Bool
	cache.rollup.observer.beforeInstall = func(builds []usageRollupBuild) {
		if len(builds) == 0 || !once.CompareAndSwap(false, true) {
			return
		}
		deleteErr := database.DeleteSession(t.Context(), "deleted-during-rollup")
		if deleteErr == nil {
			deleteErr = cache.fill.deleteNotificationSessions(
				[]string{"deleted-during-rollup"}, snapshot.ChangeRevision, snapshot.resetEpoch)
		}
		deleted <- deleteErr
	}

	require.NoError(t, database.runUsageCacheBackfill(t.Context()))
	require.NoError(t, <-deleted)
	assert.Zero(t, usageCacheCount(t, cache, "usage_cached_sessions"))
	assert.Zero(t, usageCacheCount(t, cache, "usage_rollup_installs"))
	assert.NotEmpty(t, readUsageCacheMetadata(t, cache.db)[usageCacheMetadataBackfillCompletedAt])
}

func TestUsageMatchingCountDiscoversWritesAfterCompletedBackfill(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	_, err := database.getWriter().Exec(t.Context(), `
		INSERT INTO sessions(id, project, machine, agent, started_at)
		VALUES ('external-activity', 'project', 'machine', 'claude',
		        '2026-08-12T10:00:00Z');
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, model)
		VALUES ('external-activity', 0, 'assistant', 'answer',
		        '2026-08-12T10:01:00Z', 'model')`)
	require.NoError(t, err)
	count, err := database.GetUsageMatchingSessionCount(
		t.Context(), UsageFilter{
			From: "2026-08-12", To: "2026-08-12", Timezone: "UTC",
		})
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestUsageCacheIncrementalVacuumThreshold(t *testing.T) {
	database := testDB(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)

	ran, err := cache.incrementalVacuum(t.Context(), 4096, 256)
	require.NoError(t, err)
	assert.False(t, ran)
	_, err = cache.db.ExecContext(t.Context(), `CREATE TABLE vacuum_fixture(value BLOB)`)
	require.NoError(t, err)
	for range 4200 {
		_, err = cache.db.ExecContext(t.Context(),
			`INSERT INTO vacuum_fixture VALUES (zeroblob(4096))`)
		require.NoError(t, err)
	}
	_, err = cache.db.ExecContext(t.Context(), `DELETE FROM vacuum_fixture`)
	require.NoError(t, err)
	ran, err = cache.incrementalVacuum(t.Context(), 4096, 256)
	require.NoError(t, err)
	assert.True(t, ran)
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
		require.Fail(t, "Close did not join usage cache backfill")
	}
}

func TestCloseConnectionsStopsUsageCacheBackfill(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	cache.fill.observer.beforeExtract = func([]usageSourceVersion) {
		close(started)
		<-release
	}
	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	<-started
	closed := make(chan error, 1)
	go func() { closed <- database.CloseConnections(t.Context()) }()
	var earlyErr error
	returnedEarly := false
	select {
	case earlyErr = <-closed:
		returnedEarly = true
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: absence check; the blocked backfill keeps CloseConnections waiting
	}
	close(release)
	if returnedEarly {
		require.NoError(t, earlyErr)
		require.Fail(t, "CloseConnections returned before backfill stopped")
	}
	select {
	case err = <-closed:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		require.Fail(t, "CloseConnections did not join cancelled usage cache backfill")
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
	require.NoError(t, database.InsertMessages(t.Context(), messages))

	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(
		t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)

	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))

	assert.Equal(t, usageCacheBackfillBatchSize+1,
		usageCacheCount(t, cache, "usage_rollup_installs"),
		"backfill must rebuild rollups its later batches invalidated")
	var missing int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*)
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

	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	require.NoError(t, database.Reopen())
	database.usageBackfillMu.Lock()
	done = database.usageBackfillDone
	database.usageBackfillMu.Unlock()
	require.NotNil(t, done,
		"reopen must restart backfill once it was explicitly started")
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
}

func setUsageModelPrice(t *testing.T, database *DB, model string, microdollars int64) {
	t.Helper()
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern: model,
		InputPerMTok: money.Money{Microdollars: microdollars},
	}}))
}

func TestRewarmUsageCacheStopsOldPricesBeforeAllHistoryRead(t *testing.T) {
	for _, test := range []struct {
		name   string
		rewarm bool
	}{
		{name: "rewarm", rewarm: true},
		{name: "commit-before-rewarm"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := testDB(t)
			const sessionCount = 5*usageCacheBackfillBatchSize + 1
			const refreshedPrice = int64(2_000_000)
			const costPerSession = refreshedPrice / 1_000_000 // Each seeded session has one input token.
			seedUsageBatchSessions(t, database, sessionCount,
				time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
			setUsageModelPrice(t, database, "model", 1_000_000)
			require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
			require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
			setUsageModelPrice(t, database, "model", 1_500_000)
			snapshot, err := database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindToken)
			require.NoError(t, err)
			cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			arrived := make(chan struct{}, 1)
			release := make(chan struct{})
			open := make(chan struct{})
			var once sync.Once
			openGate := func() { once.Do(func() { close(open) }) }
			t.Cleanup(func() {
				openGate()
				database.StopUsageCacheBackfill()
			})
			cache.fill.observer.afterMaintenance = func() {
				select {
				case arrived <- struct{}{}:
				case <-open:
					return
				case <-ctx.Done():
					return
				}
				select {
				case <-release:
				case <-open:
				case <-ctx.Done():
				}
			}
			require.NoError(t, database.StartUsageCacheBackfill(ctx))
			waitUsageNotification(t, arrived)
			setUsageModelPrice(t, database, "model", refreshedPrice)
			if test.rewarm {
				require.NoError(t, database.RewarmUsageCache())
			}
			daily, err := database.GetDailyUsage(ctx, UsageFilter{Progress: func(phase string) {
				if phase != "Reading cached daily totals" {
					return
				}
				// Let one pinned-price batch install after each foreground preparation.
				select {
				case release <- struct{}{}:
				case <-ctx.Done():
					require.NoError(t, ctx.Err())
				}
				waitUsageNotification(t, arrived)
			}})
			openGate()
			require.NoError(t, err)
			assert.Equal(t, sessionCount*costPerSession, daily.Totals.TotalCost.Microdollars)
			require.NoError(t, database.WaitUsageCacheBackfill(ctx))
			var rows, stale int
			require.NoError(t, cache.db.QueryRowContext(ctx, `SELECT count(*),
				count(CASE WHEN estimated_cost_microdollars != ? THEN 1 END)
				FROM usage_daily_rollups`, costPerSession).Scan(&rows, &stale))
			assert.Equal(t, sessionCount, rows)
			assert.Zero(t, stale, "every daily rollup must use the refreshed price")
		})
	}

	t.Run("commit-during-final-recheck", func(t *testing.T) {
		database := testDB(t)
		const sessionCount = usageCacheBackfillBatchSize + 1
		base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		newest := fmt.Sprintf("batch-%03d", usageCacheBackfillBatchSize)
		messages := make([]Message, 0, sessionCount)
		for i := range sessionCount {
			id := fmt.Sprintf("batch-%03d", i)
			timestamp := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
			insertSession(t, database, id, "project", func(session *Session) {
				session.StartedAt = &timestamp
				session.EndedAt = &timestamp
			})
			message := Message{
				SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: timestamp,
				Model: "model", TokenUsage: json.RawMessage(`{"input_tokens":1}`),
			}
			// Dedup peers across batches force the final recheck to rebuild
			// the newest session's invalidated install.
			if i == 0 || id == newest {
				message.ClaudeMessageID = "shared-batch-message"
				message.ClaudeRequestID = "shared-batch-request"
			}
			messages = append(messages, message)
		}
		require.NoError(t, database.InsertMessages(t.Context(), messages))
		setUsageModelPrice(t, database, "model", 1_000_000)
		snapshot, err := database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindToken)
		require.NoError(t, err)
		cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
		require.NoError(t, err)

		var newestBuilds atomic.Int64
		var changed atomic.Bool
		cache.rollup.observer.beforeInstall = func(builds []usageRollupBuild) {
			for _, build := range builds {
				if build.SessionID == newest && newestBuilds.Add(1) == 2 {
					setUsageModelPrice(t, database, "model", 2_000_000)
					changed.Store(true)
				}
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		t.Cleanup(database.StopUsageCacheBackfill)
		require.NoError(t, database.StartUsageCacheBackfill(ctx))
		require.NoError(t, database.WaitUsageCacheBackfill(ctx))
		require.True(t, changed.Load(), "commit the new price while the final recheck rebuilds installs")

		// Inspect persisted rows before any foreground read can repair them.
		var rows, stale int
		require.NoError(t, cache.db.QueryRowContext(ctx, `SELECT count(*),
			count(CASE WHEN estimated_cost_microdollars != 2 THEN 1 END)
			FROM usage_daily_rollups`).Scan(&rows, &stale))
		assert.Equal(t, sessionCount-2, rows, "dedup peers remain in exception groups")
		assert.Zero(t, stale, "a price committed during the final recheck must trigger a fresh pass")
	})

	t.Run("commit-during-last-batch", func(t *testing.T) {
		database := testDB(t)
		const sessionCount = usageCacheBackfillBatchSize + 1
		seedUsageBatchSessions(t, database, sessionCount,
			time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
		setUsageModelPrice(t, database, "model", 1_000_000)
		snapshot, err := database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindToken)
		require.NoError(t, err)
		cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
		require.NoError(t, err)

		var changed atomic.Bool
		cache.rollup.observer.beforeInstall = func(builds []usageRollupBuild) {
			for _, build := range builds {
				// The oldest session is alone in the final, newest-first batch.
				if build.SessionID == "batch-000" && changed.CompareAndSwap(false, true) {
					setUsageModelPrice(t, database, "model", 2_000_000)
				}
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		t.Cleanup(database.StopUsageCacheBackfill)
		require.NoError(t, database.StartUsageCacheBackfill(ctx))
		require.NoError(t, database.WaitUsageCacheBackfill(ctx))
		require.True(t, changed.Load(), "commit the new price during the last batch")

		// Read persisted rows directly so a foreground read cannot repair stale prices.
		var rows, stale int
		require.NoError(t, cache.db.QueryRowContext(ctx, `SELECT count(*),
			count(CASE WHEN estimated_cost_microdollars != 2 THEN 1 END)
			FROM usage_daily_rollups`).Scan(&rows, &stale))
		assert.Equal(t, sessionCount, rows)
		assert.Zero(t, stale, "the final pricing check must restart with the committed price")
	})
}

func TestRewarmUsageCacheRerunsAfterActivePassWithNewPrices(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "rewarm", "project", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "rewarm", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "rewarm-model",
		TokenUsage: json.RawMessage(`{"input_tokens":1000000}`),
	}}))
	setUsageModelPrice(t, database, "rewarm-model", 1_000_000)

	require.NoError(t, database.RewarmUsageCache())
	database.usageBackfillMu.Lock()
	done := database.usageBackfillDone
	database.usageBackfillMu.Unlock()
	require.Nil(t, done, "re-warm must not start backfill nothing enabled")

	require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)

	// Give the first re-warm stale prices so it reaches the build hook.
	setUsageModelPrice(t, database, "rewarm-model", 2_000_000)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	cache.rollup.observer.beforeEnsure = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	require.NoError(t, database.RewarmUsageCache())
	<-entered
	setUsageModelPrice(t, database, "rewarm-model", 3_000_000)
	require.NoError(t, database.RewarmUsageCache())
	close(release)
	require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
	cache.rollup.observer = usageRollupObserver{}

	rows, err := cache.db.QueryContext(t.Context(),
		`SELECT DISTINCT estimated_cost_microdollars FROM usage_daily_rollups`)
	require.NoError(t, err)
	defer rows.Close()
	var costs []int64
	for rows.Next() {
		var cost int64
		require.NoError(t, rows.Scan(&cost))
		costs = append(costs, cost)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []int64{3_000_000}, costs,
		"the queued pass must rebuild with the price committed during the first")
}

func TestUsagePricingDigestChangesWithCommittedPricing(t *testing.T) {
	database := testDB(t)
	before, err := database.UsagePricingDigest(t.Context())
	require.NoError(t, err)
	unchanged, err := database.UsagePricingDigest(t.Context())
	require.NoError(t, err)
	assert.Equal(t, before, unchanged)
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "digest-model", InputPerMTok: money.Money{Microdollars: 1},
	}}))
	after, err := database.UsagePricingDigest(t.Context())
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
}

func TestUsageBackfillJournalCapture(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"one", "two", "three"} {
		insertSession(t, database, id, "project")
		require.NoError(t, database.InsertMessages(t.Context(), []Message{{
			SessionID: id, Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:00:00Z", Model: "model",
			TokenUsage: json.RawMessage(`{"input_tokens":1}`),
		}}))
	}
	_, err := database.GetDailyUsage(t.Context(), UsageFilter{Timezone: "Etc/GMT+3"})
	require.NoError(t, err)
	require.NoError(t, database.runUsageCacheBackfill(t.Context()))
	id, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	capture := func() (usageQuerySnapshot, bool) {
		var seq int64
		require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT CAST(value AS INTEGER) FROM usage_cache_metadata WHERE key = ?`, usageCacheMetadataNextRollupRevision).Scan(&seq))
		snapshot, locations, _, delta, err := database.captureUsageBackfill(t.Context(), cache, id, seq)
		require.NoError(t, err)
		require.Len(t, locations, 2)
		return snapshot, delta
	}
	snapshot, delta := capture()
	require.True(t, delta)
	require.Empty(t, snapshot.Sessions)
	_, err = cache.db.ExecContext(t.Context(), `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES ('marker', NULL, '1')`)
	require.NoError(t, err)
	require.NoError(t, database.runUsageCacheBackfill(t.Context()))
	var markers int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_stat1 WHERE tbl = 'marker'`).Scan(&markers))
	assert.Equal(t, 1, markers)
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "two", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-11T09:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":2}`),
	}}))
	current, err := database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindActivity)
	require.NoError(t, err)
	_, err = cache.db.ExecContext(t.Context(), `UPDATE usage_rollup_timezones SET change_revision = ? WHERE timezone_key = ?`, current.ChangeRevision, usageTimezoneIdentityFor((UsageFilter{}).Location(), nil).Key)
	require.NoError(t, err)
	snapshot, delta = capture()
	require.False(t, delta)
	require.Len(t, snapshot.Sessions, 3)
	require.NoError(t, database.runUsageCacheBackfill(t.Context()))
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_stat1 WHERE tbl = 'marker'`).Scan(&markers))
	assert.Zero(t, markers)
	snapshot, delta = capture()
	require.True(t, delta)
	require.Empty(t, snapshot.Sessions)
	_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs WHERE session_id = 'one'`)
	require.NoError(t, err)
	snapshot, delta = capture()
	require.False(t, delta)
	require.Len(t, snapshot.Sessions, 3)
	require.NoError(t, database.runUsageCacheBackfill(t.Context()))
	require.NoError(t, database.DeleteSession(t.Context(), "three"))
	snapshot, delta = capture()
	require.False(t, delta)
	require.Len(t, snapshot.Sessions, 2)
	require.NoError(t, database.runUsageCacheBackfill(t.Context()))
	_, err = cache.db.ExecContext(t.Context(), `UPDATE usage_rollup_timezones SET change_revision = NULL`)
	require.NoError(t, err)
	snapshot, delta = capture()
	require.False(t, delta)
	assert.Len(t, snapshot.Sessions, 2)
}

func TestUsageBackfillCaptureExternalSource(t *testing.T) {
	database := testDB(t)
	seedUsageSnapshotSession(t, database, "rollup-race", "project-a", "2026-08-10T09:00:00Z", 0, 10, "model-a")
	id, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	source := &changingUsageSource{rollupAccountSource: &rollupAccountSource{database: database, databaseID: id, revision: 10, output: 10}}
	reader, err := NewUsageCacheReader(t.TempDir(), source)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	_, err = reader.GetDailyUsage(t.Context(), UsageFilter{})
	require.NoError(t, err)
	cache, err := reader.engine.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	zone, err := readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor((UsageFilter{}).Location(), nil).Key)
	require.NoError(t, err)
	require.True(t, zone.revision.Valid)
	snapshot, _, _, delta, err := reader.engine.captureUsageBackfill(t.Context(), cache, id, 0)
	require.NoError(t, err)
	assert.False(t, delta)
	require.Len(t, snapshot.Sessions, 1)
	assert.Equal(t, "rollup-race", snapshot.Sessions[0].ID)
}

func capturedUsageBackfillCoverage(t *testing.T, database *DB, snapshot usageQuerySnapshot, installSeq int64) ([]*time.Location, []usageZoneCoverage) {
	t.Helper()
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	locations, err := usageBackfillLocations(t.Context(), cache, snapshot.location)
	require.NoError(t, err)
	zones := make([]usageZoneCoverage, len(locations))
	for i, location := range locations {
		zones[i], err = readUsageZoneCoverage(t.Context(), cache.db, usageTimezoneIdentityFor(location, nil).Key)
		require.NoError(t, err)
		zones[i].installSeq = installSeq
		zones[i].resetEpoch = snapshot.resetEpoch
	}
	return locations, zones
}

func runCapturedUsageBackfill(t *testing.T, database *DB, snapshot usageQuerySnapshot, installSeq int64) error {
	t.Helper()
	locations, zones := capturedUsageBackfillCoverage(t, database, snapshot, installSeq)
	return database.runUsageCacheBackfillWithCoverage(t.Context(), time.Now(), snapshot, installSeq, locations, zones, false)
}

func TestUsageBackfillAppendSharedIdentity(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"one", "two"} {
		insertSession(t, database, id, "project", func(session *Session) {
			session.Agent = "claude"
		})
	}
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "one", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":10}`), ClaudeMessageID: "shared", ClaudeRequestID: "request",
	}}))
	require.NoError(t, database.runUsageCacheBackfill(t.Context()))
	id, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), id)
	require.NoError(t, err)
	var originalTokens int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT r.input_tokens
		FROM usage_daily_rollups r JOIN usage_rollup_installs i ON i.id = r.rollup_install_id
		WHERE i.session_id = 'one'`).Scan(&originalTokens))
	require.Equal(t, 10, originalTokens)
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "two", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T09:01:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":20}`), ClaudeMessageID: "shared", ClaudeRequestID: "request",
	}}))
	require.NoError(t, database.runUsageCacheBackfill(t.Context()))
	// Read the installed rows before a foreground query can repair the cache.
	rows, err := cache.db.QueryContext(t.Context(), `SELECT i.session_id, e.input_tokens
		FROM usage_rollup_exceptions e JOIN usage_rollup_installs i ON i.id = e.rollup_install_id
		WHERE e.claude_message_id = 'shared' AND e.claude_request_id = 'request'
		ORDER BY i.session_id`)
	require.NoError(t, err)
	defer rows.Close()
	var sessions []string
	var tokens []int
	for rows.Next() {
		var session string
		var count int
		require.NoError(t, rows.Scan(&session, &count))
		sessions = append(sessions, session)
		tokens = append(tokens, count)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	assert.Equal(t, []string{"one", "two"}, sessions)
	assert.Equal(t, []int{10, 20}, tokens)
	var invalidations int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*) FROM usage_rollup_invalidations`).Scan(&invalidations))
	assert.Zero(t, invalidations)
}
