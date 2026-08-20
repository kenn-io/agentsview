//go:build fts5

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/usagefacts"
)

func TestUsageCacheFillInstallsCompleteSessionFacts(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "fill-session", "project", func(s *Session) {
		s.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages([]Message{
		{SessionID: "fill-session", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-07-01T09:00:00Z", Model: "model-old",
			TokenUsage:      json.RawMessage(`{"input_tokens":2,"output_tokens":3}`),
			ClaudeMessageID: "message-id", ClaudeRequestID: "request-id",
			SourceUUID: "source-id"},
		{SessionID: "fill-session", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-08-10T09:00:00Z", Model: "model-new"},
	}))
	require.NoError(t, database.ReplaceSessionUsageEvents("fill-session", []UsageEvent{{
		Source: "session", Model: "event-model", InputTokens: 5,
		OccurredAt: "2026-06-01T09:00:00Z", DedupKey: "event-1",
	}}))

	snapshot, err := database.captureUsageQuery(context.Background(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	}, usageQueryKindActivity)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	results, err := cache.fill.Ensure(
		context.Background(), snapshot.Versions, snapshot.CursorHighWater,
	)
	require.NoError(t, err)
	assert.False(t, results["fill-session"].Deleted)
	assert.Positive(t, results["fill-session"].InstallRevision)

	rows, err := cache.db.Query(`
		SELECT source, message_ordinal, model, token_eligible, activity_eligible,
		       claude_message_id, claude_request_id, source_uuid
		FROM usage_facts ORDER BY fact_index`)
	require.NoError(t, err)
	defer rows.Close()
	type row struct {
		source, model                    string
		ordinal                          *int
		token, active                    int
		messageID, requestID, sourceUUID string
	}
	var got []row
	for rows.Next() {
		var item row
		require.NoError(t, rows.Scan(
			&item.source, &item.ordinal, &item.model, &item.token, &item.active,
			&item.messageID, &item.requestID, &item.sourceUUID,
		))
		got = append(got, item)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 3)
	assert.Equal(t, []string{"model-old", "model-new", "event-model"},
		[]string{got[0].model, got[1].model, got[2].model})
	assert.Equal(t, []int{1, 0, 1}, []int{got[0].token, got[1].token, got[2].token})
	assert.Equal(t, "message-id", got[0].messageID)
	assert.Equal(t, "request-id", got[0].requestID)
	assert.Equal(t, "source-id", got[0].sourceUUID)
}

func TestUsageCacheFillExtractionUsesNarrowIndexes(t *testing.T) {
	database := usageCandidateFixture(t)
	conn, err := database.getReader().Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(context.Background(), `
		CREATE TEMP TABLE usage_fill_sessions(
			session_id TEXT PRIMARY KEY
		) WITHOUT ROWID;
		INSERT INTO usage_fill_sessions VALUES ('inside-message')`)
	require.NoError(t, err)

	plan := func(query string) string {
		t.Helper()
		rows, queryErr := conn.QueryContext(
			context.Background(), `EXPLAIN QUERY PLAN `+query,
		)
		require.NoError(t, queryErr)
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
	assert.Contains(t, plan(usageFillMessageFactsSQL),
		"SEARCH m USING COVERING INDEX idx_messages_usage_session_covering")
	assert.Contains(t, plan(usageFillActivityFactsSQL),
		"idx_messages_session_role")
	assert.Contains(t, plan(usageFillEventFactsSQL),
		"idx_usage_events_session")
}

func TestUsageFillCoordinatorSharesDetachedSessionWork(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)

	started := make(chan struct{})
	release := make(chan struct{})
	var extractions atomic.Int32
	cache.fill.observer = usageFillObserver{
		beforeExtract: func([]usageSourceVersion) {
			if extractions.Add(1) == 1 {
				close(started)
				<-release
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, fillErr := cache.fill.Ensure(ctx, snapshot.Versions[:1], 0)
		firstDone <- fillErr
	}()
	<-started
	secondDone := make(chan error, 1)
	go func() {
		_, fillErr := cache.fill.Ensure(
			context.Background(), snapshot.Versions[:1], 0,
		)
		secondDone <- fillErr
	}()
	cancel()
	close(release)
	assert.ErrorIs(t, <-firstDone, context.Canceled)
	require.NoError(t, <-secondDone)
	assert.Equal(t, int32(1), extractions.Load())
}

func TestUsageFillCoordinatorDoesNotJoinOlderSourceVersion(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	oldVersion := snapshot.Versions[0]
	oldStarted := make(chan struct{})
	newStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) {
		if versions[0].TranscriptRevision == oldVersion.TranscriptRevision {
			select {
			case <-oldStarted:
			default:
				close(oldStarted)
			}
			<-releaseOld
			return
		}
		select {
		case <-newStarted:
		default:
			close(newStarted)
		}
	}
	oldDone := make(chan error, 1)
	go func() {
		_, fillErr := cache.fill.Ensure(
			context.Background(), []usageSourceVersion{oldVersion}, 0)
		oldDone <- fillErr
	}()
	<-oldStarted
	_, err = database.getWriter().Exec(`
		UPDATE sessions SET transcript_revision = 'newer' WHERE id = ?`,
		oldVersion.SessionID)
	require.NoError(t, err)
	current, err := cache.fill.recheckSourceVersions(
		context.Background(), []usageSourceVersion{{SessionID: oldVersion.SessionID}})
	require.NoError(t, err)
	newVersion := current[oldVersion.SessionID]
	newDone := make(chan error, 1)
	go func() {
		_, fillErr := cache.fill.Ensure(
			context.Background(), []usageSourceVersion{newVersion}, 0)
		newDone <- fillErr
	}()
	select {
	case <-newStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("newer source version joined an older in-flight fill")
	}
	close(releaseOld)
	require.NoError(t, <-newDone)
	require.ErrorIs(t, <-oldDone, errUsageCacheSourceChanged)
}

func TestUsageFillRecheckRestoresReaderBusyTimeout(t *testing.T) {
	database := usageCandidateFixture(t)
	database.reader.Load().SetMaxOpenConns(1)
	conn, err := database.getReader().Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `PRAGMA busy_timeout=4321`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.fill.recheckSourceVersions(context.Background(), snapshot.Versions[:1])
	require.NoError(t, err)
	var timeout int
	require.NoError(t, database.getReader().QueryRow(
		`PRAGMA busy_timeout`).Scan(&timeout))
	assert.Equal(t, 4321, timeout)
}

func TestUsageCacheFillReportsDeletedSession(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	version := snapshot.Versions[0]

	var deleteErr error
	cache.fill.observer = usageFillObserver{
		afterExtract: func([]usageSourceVersion) {
			_, deleteErr = database.getWriter().Exec(
				`DELETE FROM sessions WHERE id = ?`, version.SessionID,
			)
		},
	}
	results, err := cache.fill.Ensure(context.Background(), []usageSourceVersion{version}, 0)
	require.NoError(t, err)
	require.NoError(t, deleteErr)
	assert.True(t, results[version.SessionID].Deleted)
}

func TestUsageCacheFillRetriesChangedFingerprint(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	version := snapshot.Versions[0]
	var mutations atomic.Int32
	var mutationErr atomic.Value
	cache.fill.observer = usageFillObserver{
		afterExtract: func([]usageSourceVersion) {
			if mutations.Add(1) != 1 {
				return
			}
			_, updateErr := database.getWriter().Exec(`
				UPDATE sessions SET transcript_revision = 'later'
				WHERE id = ?`, version.SessionID)
			if updateErr != nil {
				mutationErr.Store(updateErr)
			}
		},
	}
	results, err := cache.fill.Ensure(
		context.Background(), []usageSourceVersion{version}, 0,
	)
	require.ErrorIs(t, err, errUsageCacheSourceChanged)
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(t, stored.(error))
	}
	assert.Equal(t, int32(2), mutations.Load())
	assert.Nil(t, results)
}

func TestDailyUsageRecapturesChangedFillSource(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "moving", "keep", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "moving", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	var mutations atomic.Int32
	var mutationErr atomic.Value
	cache.fill.observer.afterExtract = func([]usageSourceVersion) {
		if mutations.Add(1) != 1 {
			return
		}
		_, updateErr := database.getWriter().Exec(`
			UPDATE messages SET token_usage = '{"input_tokens":9}'
			WHERE session_id = 'moving';
			UPDATE sessions SET project = 'drop', transcript_revision = 'later'
			WHERE id = 'moving'`)
		if updateErr != nil {
			mutationErr.Store(updateErr)
		}
	}
	daily, err := database.GetDailyUsage(context.Background(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
		Project: "keep", SkipSessionCounts: true,
	})
	require.NoError(t, err)
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(t, stored.(error))
	}
	assert.Zero(t, daily.Totals.InputTokens)
}

func TestUsageCacheFillBoundsMovingFingerprintRetries(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	version := snapshot.Versions[0]
	var mutations atomic.Int32
	cache.fill.observer = usageFillObserver{
		afterExtract: func([]usageSourceVersion) {
			n := mutations.Add(1)
			_, _ = database.getWriter().Exec(`
				UPDATE sessions SET transcript_revision = ? WHERE id = ?`,
				fmt.Sprintf("moving-%d", n), version.SessionID)
		},
	}
	_, err = cache.fill.Ensure(context.Background(), []usageSourceVersion{version}, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"usage cache fill could not verify archive state after 3 attempts")
	assert.Equal(t, int32(3), mutations.Load())
}

func TestUsageCacheFillTwoHandlesRaceIdempotently(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	first, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	secondManager := newUsageCacheManager(database.path)
	secondManager.attachArchive(database)
	t.Cleanup(func() { require.NoError(t, secondManager.Close()) })
	second, err := secondManager.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)

	errorsCh := make(chan error, 2)
	for _, cache := range []*usageCache{first, second} {
		go func(cache *usageCache) {
			_, fillErr := cache.fill.Ensure(context.Background(), snapshot.Versions, 0)
			errorsCh <- fillErr
		}(cache)
	}
	require.NoError(t, <-errorsCh)
	require.NoError(t, <-errorsCh)
	assert.Equal(t, len(snapshot.Versions),
		usageCacheCount(t, first, "usage_cached_sessions"))
}

func TestUsageCacheFillTokenCoverageRepairIsByteEquivalent(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "repair", "project", func(s *Session) {
		s.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages([]Message{{
		SessionID: "repair", Ordinal: 0, Role: "assistant", Model: "model",
		Timestamp:  "2026-08-10T09:00:00Z",
		TokenUsage: json.RawMessage(`{"input_tokens":2,"output_tokens":3}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(context.Background(), snapshot.Versions, 0)
	require.NoError(t, err)
	before := dumpCachedFacts(t, cache, "repair")

	_, err = database.getWriter().Exec(`
		UPDATE messages SET has_context_tokens = 0, has_output_tokens = 0
		WHERE session_id = 'repair';
		DELETE FROM stats WHERE key = ?`, tokenCoverageRepairStatsKey)
	require.NoError(t, err)
	database.mu.Lock()
	require.NoError(t, database.backfillTokenCoverageFlagsLocked(database.getWriter()))
	require.NoError(t, database.markTokenCoverageRepairDoneLocked(database.getWriter()))
	database.mu.Unlock()
	afterRepair, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(context.Background(), afterRepair.Versions, 0)
	require.NoError(t, err)
	assert.Equal(t, before, dumpCachedFacts(t, cache, "repair"))
}

func TestUsageCursorFactsResumeAtRequestedHighWater(t *testing.T) {
	database := usageCandidateFixture(t)
	require.NoError(t, database.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt: "2026-08-11T00:00:00Z", Model: "cursor-2",
		InputTokens: 4, DedupKey: "cursor-2",
	}}))
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)

	_, err = cache.fill.Ensure(context.Background(), nil, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, usageCacheCount(t, cache, "cursor_usage_facts"))
	_, err = cache.fill.Ensure(context.Background(), nil, snapshot.CursorHighWater)
	require.NoError(t, err)
	assert.Equal(t, 2, usageCacheCount(t, cache, "cursor_usage_facts"))
}

func TestUsageCursorFactsRetainBoundedBatchProgress(t *testing.T) {
	database := testDB(t)
	const batchSize = usageCursorCopyBatchSize
	events := make([]CursorUsageEvent, 0, batchSize+1)
	for index := range batchSize + 1 {
		events = append(events, CursorUsageEvent{
			OccurredAt: "2026-08-11T00:00:00Z", Model: "cursor-model",
			InputTokens: 1, DedupKey: fmt.Sprintf("cursor-batch-%04d", index),
		})
	}
	require.NoError(t, database.InsertCursorUsageEvents(events))
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.db.Exec(fmt.Sprintf(`
		CREATE TRIGGER reject_late_cursor_fact
		BEFORE INSERT ON cursor_usage_facts
		WHEN NEW.source_id > %d
		BEGIN
			SELECT RAISE(ABORT, 'stop after one bounded batch');
		END`, batchSize))
	require.NoError(t, err)

	_, err = cache.fill.Ensure(context.Background(), nil, snapshot.CursorHighWater)
	require.Error(t, err)
	assert.Equal(t, batchSize, usageCacheCount(t, cache, "cursor_usage_facts"))
	assert.Equal(t, fmt.Sprint(batchSize), readUsageCacheMetadata(
		t, cache.db)[usageCacheMetadataCursorHighWaterMark])

	_, err = cache.db.Exec(`DROP TRIGGER reject_late_cursor_fact`)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(context.Background(), nil, snapshot.CursorHighWater)
	require.NoError(t, err)
	assert.Equal(t, batchSize+1, usageCacheCount(t, cache, "cursor_usage_facts"))
}

func TestUsageCursorFactsRejectChangedArchiveGeneration(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = database.getWriter().Exec(`
		UPDATE archive_metadata SET value = 'replacement-database'
		WHERE key = ?`, archiveMetadataDatabaseIDKey)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(context.Background(), nil, snapshot.CursorHighWater)
	require.ErrorIs(t, err, errUsageCacheSourceChanged)
	assert.Zero(t, usageCacheCount(t, cache, "cursor_usage_facts"))
}

func TestUsageCursorFactsNormalizeRequestCounters(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt: "2026-08-11T00:00:00Z", Model: "cursor-model",
		InputTokens: -1, OutputTokens: usagefacts.MaxPlausibleTokens + 1,
		DedupKey: "cursor-normalized",
	}}))
	daily, err := database.GetDailyUsage(context.Background(), UsageFilter{})
	require.NoError(t, err)
	assert.Zero(t, daily.Totals.InputTokens)
	assert.Equal(t, usagefacts.MaxPlausibleTokens, daily.Totals.OutputTokens)
}

func TestUsageCacheNotificationIsNonblocking(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	blocked := make(chan struct{})
	cache.fill.observer = usageFillObserver{
		beforeExtract: func([]usageSourceVersion) { <-blocked },
	}

	done := make(chan struct{})
	go func() {
		database.usageCache.NotifySessions([]string{snapshot.Versions[0].SessionID})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("usage cache notification blocked on fill work")
	}
	close(blocked)
}

func TestUsageCacheNotificationsCoalesceChangedSessions(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"notify-a", "notify-b"} {
		insertSession(t, database, id, "project")
		require.NoError(t, database.InsertMessages([]Message{{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-10T09:00:00Z", Model: "model",
			TokenUsage: json.RawMessage(`{"input_tokens":1}`),
		}}))
	}
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(t, err)

	batch := make(chan []usageSourceVersion, 1)
	cache.fill.observer = usageFillObserver{
		beforeExtract: func(versions []usageSourceVersion) { batch <- versions },
	}
	_, err = database.getWriter().Exec(`UPDATE sessions
		SET transcript_revision = '2'
		WHERE id IN ('notify-a', 'notify-b')`)
	require.NoError(t, err)
	database.usageCache.NotifySessions([]string{"notify-a"})
	database.usageCache.NotifySessions([]string{"notify-a"})
	database.usageCache.NotifySessions([]string{"notify-b"})

	select {
	case versions := <-batch:
		require.Len(t, versions, 2)
	case <-time.After(30 * time.Second):
		t.Fatal("coalesced usage cache fill did not start")
	}
	select {
	case versions := <-batch:
		t.Fatalf("duplicate notification started another fill: %#v", versions)
	case <-time.After(3 * usageFillNotificationDebounce):
	}
}

func TestUsageCacheNotificationObservesOnlyCommittedWrites(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "notify", "project", func(s *Session) {
		s.StartedAt = &started
	})
	initial := Message{
		SessionID: "notify", Ordinal: 0, Role: "assistant", Model: "model",
		Timestamp:  "2026-08-10T09:00:00Z",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}
	require.NoError(t, database.InsertMessages([]Message{initial}))
	snapshot, err := database.captureUsageQuery(
		context.Background(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(context.Background(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(context.Background(), snapshot.Versions, 0)
	require.NoError(t, err)

	observed := make(chan string, 1)
	cache.fill.observer = usageFillObserver{
		beforeExtract: func([]usageSourceVersion) {
			var tokenUsage string
			queryErr := database.getReader().QueryRow(`
				SELECT token_usage FROM messages
				WHERE session_id = 'notify' AND ordinal = 0`).Scan(&tokenUsage)
			if queryErr != nil {
				observed <- queryErr.Error()
				return
			}
			observed <- tokenUsage
		},
	}
	updated := initial
	updated.TokenUsage = json.RawMessage(`{"input_tokens":9}`)
	require.NoError(t, database.ReplaceSessionMessages("notify", []Message{updated}))
	select {
	case got := <-observed:
		assert.JSONEq(t, `{"input_tokens":9}`, got)
	case <-time.After(30 * time.Second):
		t.Fatal("committed message write did not notify the usage cache")
	}

	duplicate := updated
	duplicate.TokenUsage = json.RawMessage(`{"input_tokens":10}`)
	require.Error(t, database.InsertMessages([]Message{duplicate}))
	select {
	case value := <-observed:
		t.Fatalf("rolled-back message write emitted usage notification: %s", value)
	case <-time.After(100 * time.Millisecond):
	}
}

func usageCacheCount(t *testing.T, cache *usageCache, table string) int {
	t.Helper()
	var count int
	require.NoError(t, cache.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
	return count
}

func dumpCachedFacts(t *testing.T, cache *usageCache, sessionID string) [][]any {
	t.Helper()
	rows, err := cache.db.Query(`
		SELECT f.fact_index, f.source, f.message_ordinal, f.timestamp_ms,
		       f.raw_timestamp, f.uses_session_start, f.model,
		       f.input_tokens, f.output_tokens, f.reasoning_tokens,
		       f.cache_creation_tokens, f.cache_read_tokens,
		       f.web_search_requests, f.reported_cost_microdollars,
		       f.cost_source, f.request_scoped, f.claude_message_id,
		       f.claude_request_id, f.source_uuid, f.usage_dedup_key,
		       f.token_eligible, f.activity_eligible
		FROM usage_facts f
		JOIN usage_cached_sessions s ON s.id = f.cached_session_id
		WHERE s.session_id = ? ORDER BY f.fact_index`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var result [][]any
	for rows.Next() {
		values := make([]any, 22)
		pointers := make([]any, len(values))
		for index := range values {
			pointers[index] = &values[index]
		}
		require.NoError(t, rows.Scan(pointers...))
		result = append(result, values)
	}
	require.NoError(t, rows.Err())
	return result
}
