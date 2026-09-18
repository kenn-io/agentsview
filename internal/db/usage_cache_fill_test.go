//go:build fts5

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/usagefacts"
)

func TestUsageCacheFillInstallsCompleteSessionFacts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "fill-session", "project", func(s *Session) {
		s.StartedAt = &started
	})
	require.NoError(database.InsertMessages([]Message{
		{
			SessionID: "fill-session", Ordinal: 0, Role: "assistant",
			Timestamp: "2026-07-01T09:00:00Z", Model: "model-old",
			TokenUsage:      json.RawMessage(`{"input_tokens":2,"output_tokens":3}`),
			ClaudeMessageID: "message-id", ClaudeRequestID: "request-id",
			SourceUUID: "source-id",
		},
		{
			SessionID: "fill-session", Ordinal: 1, Role: "assistant",
			Timestamp: "2026-08-10T09:00:00Z", Model: "model-new",
		},
	}))
	require.NoError(database.ReplaceSessionUsageEvents("fill-session", []UsageEvent{{
		Source: "session", Model: "event-model", InputTokens: 5,
		OccurredAt: "2026-06-01T09:00:00Z", DedupKey: "event-1",
	}}))

	snapshot, err := database.captureUsageQuery(t.Context(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	}, usageQueryKindActivity)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	results, err := cache.fill.Ensure(
		t.Context(), snapshot.Versions, snapshot.CursorHighWater,
	)
	require.NoError(err)
	assert.False(results["fill-session"].Deleted)
	assert.Positive(results["fill-session"].InstallRevision)

	rows, err := cache.db.QueryContext(t.Context(), `
		SELECT source, message_ordinal, model, token_eligible, activity_eligible,
		       claude_message_id, claude_request_id, source_uuid
		FROM usage_facts ORDER BY fact_index`)
	require.NoError(err)
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
		require.NoError(rows.Scan(
			&item.source, &item.ordinal, &item.model, &item.token, &item.active,
			&item.messageID, &item.requestID, &item.sourceUUID,
		))
		got = append(got, item)
	}
	require.NoError(rows.Err())
	require.Len(got, 3)
	assert.Equal([]string{"model-old", "model-new", "event-model"},
		[]string{got[0].model, got[1].model, got[2].model})
	assert.Equal([]int{1, 0, 1}, []int{got[0].token, got[1].token, got[2].token})
	assert.Equal("message-id", got[0].messageID)
	assert.Equal("request-id", got[0].requestID)
	assert.Equal("source-id", got[0].sourceUUID)
}

func TestUsageCacheFillExtractionUsesNarrowIndexes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := usageCandidateFixture(t)
	conn, err := database.getReader().Conn(t.Context())
	require.NoError(err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `
		CREATE TEMP TABLE usage_fill_sessions(
			session_id TEXT PRIMARY KEY
		) WITHOUT ROWID;
		INSERT INTO usage_fill_sessions VALUES ('inside-message')`)
	require.NoError(err)

	plan := func(query string) string {
		t.Helper()
		rows, queryErr := conn.QueryContext(
			t.Context(), `EXPLAIN QUERY PLAN `+query,
		)
		require.NoError(queryErr)
		defer rows.Close()
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(rows.Scan(&id, &parent, &unused, &detail))
			details = append(details, detail)
		}
		require.NoError(rows.Err())
		return strings.Join(details, "\n")
	}
	assert.Contains(plan(usageFillMessageFactsSQL),
		"SEARCH m USING COVERING INDEX idx_messages_usage_session_covering")
	assert.Contains(plan(usageFillActivityFactsSQL),
		"idx_messages_session_role")
	assert.Contains(plan(usageFillEventFactsSQL),
		"idx_usage_events_session")
}

func TestUsageFillCoordinatorSharesDetachedSessionWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)

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
	ctx, cancel := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() {
		_, fillErr := cache.fill.Ensure(ctx, snapshot.Versions[:1], 0)
		firstDone <- fillErr
	}()
	<-started
	secondDone := make(chan error, 1)
	go func() {
		_, fillErr := cache.fill.Ensure(
			t.Context(), snapshot.Versions[:1], 0,
		)
		secondDone <- fillErr
	}()
	cancel()
	close(release)
	assert.ErrorIs(<-firstDone, context.Canceled)
	require.NoError(<-secondDone)
	assert.Equal(int32(1), extractions.Load())
}

func TestUsageFillCoordinatorDoesNotJoinOlderSourceVersion(t *testing.T) {
	require := require.New(t)

	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
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
			t.Context(), []usageSourceVersion{oldVersion}, 0)
		oldDone <- fillErr
	}()
	<-oldStarted
	_, err = database.getWriter().Exec(`
		UPDATE sessions SET transcript_revision = 'newer' WHERE id = ?`,
		oldVersion.SessionID)
	require.NoError(err)
	current, err := cache.fill.recheckSourceVersions(
		t.Context(), []usageSourceVersion{{SessionID: oldVersion.SessionID}})
	require.NoError(err)
	newVersion := current[oldVersion.SessionID]
	newDone := make(chan error, 1)
	go func() {
		_, fillErr := cache.fill.Ensure(
			t.Context(), []usageSourceVersion{newVersion}, 0)
		newDone <- fillErr
	}()
	select {
	case <-newStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("newer source version joined an older in-flight fill")
	}
	close(releaseOld)
	require.NoError(<-newDone)
	// The requested version still keys the single-flight call, so the newer
	// version cannot join the older one's in-flight fill. The older caller
	// itself no longer fails: its extraction reads whatever the archive holds
	// when its own read transaction opens, and it reports that version back.
	require.NoError(<-oldDone)
}

func TestUsageFillOlderExtractionCannotReplaceNewerCachedFacts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	var oldVersion usageSourceVersion
	for _, version := range snapshot.Versions {
		if version.SessionID == "inside-message" {
			oldVersion = version
			break
		}
	}
	require.Equal("inside-message", oldVersion.SessionID)
	oldExtracted := make(chan struct{})
	releaseOld := make(chan struct{})
	var oldExtractions atomic.Int32
	cache.fill.observer.afterExtract = func(versions []usageSourceVersion) {
		if versions[0].TranscriptRevision != oldVersion.TranscriptRevision {
			return
		}
		if oldExtractions.Add(1) == 1 {
			close(oldExtracted)
			<-releaseOld
		}
	}
	type fillOutcome struct {
		results map[string]usageFillResult
		err     error
	}
	oldDone := make(chan fillOutcome, 1)
	go func() {
		results, fillErr := cache.fill.Ensure(
			t.Context(), []usageSourceVersion{oldVersion}, 0)
		oldDone <- fillOutcome{results: results, err: fillErr}
	}()
	<-oldExtracted
	tx, err := database.getWriter().BeginTx(t.Context(), nil)
	require.NoError(err)
	_, err = tx.ExecContext(t.Context(), `UPDATE messages SET token_usage = '{"input_tokens":9}'
		WHERE session_id = ?`, oldVersion.SessionID)
	require.NoError(err)
	_, err = tx.ExecContext(t.Context(), `UPDATE sessions SET transcript_revision = 'newer'
		WHERE id = ?`, oldVersion.SessionID)
	require.NoError(err)
	require.NoError(tx.Commit())
	current, err := cache.fill.recheckSourceVersions(
		t.Context(), []usageSourceVersion{{SessionID: oldVersion.SessionID}})
	require.NoError(err)
	newVersion := current[oldVersion.SessionID]
	newResults, err := cache.fill.Ensure(
		t.Context(), []usageSourceVersion{newVersion}, 0)
	require.NoError(err)
	close(releaseOld)
	oldOutcome := <-oldDone
	require.NoError(oldOutcome.err)

	assert.Equal(newVersion, newResults[oldVersion.SessionID].source)
	assert.Equal(newVersion, oldOutcome.results[oldVersion.SessionID].source)
	assert.Equal(int32(2), oldExtractions.Load(),
		"the older fill should re-extract after losing the cache race")
	var inputTokens int64
	require.NoError(cache.db.QueryRowContext(t.Context(), `SELECT f.input_tokens
		FROM usage_facts f JOIN usage_cached_sessions s
		  ON s.id = f.cached_session_id
		WHERE s.session_id = ?`, oldVersion.SessionID).Scan(&inputTokens))
	assert.Equal(int64(9), inputTokens)
}

func TestUsageFillRecheckRestoresReaderBusyTimeout(t *testing.T) {
	require := require.New(t)

	database := usageCandidateFixture(t)
	database.reader.Load().SetMaxOpenConns(1)
	conn, err := database.getReader().Conn(t.Context())
	require.NoError(err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA busy_timeout=4321`)
	require.NoError(err)
	require.NoError(conn.Close())
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	_, err = cache.fill.recheckSourceVersions(t.Context(), snapshot.Versions[:1])
	require.NoError(err)
	var timeout int
	require.NoError(database.getReader().QueryRow(
		`PRAGMA busy_timeout`).Scan(&timeout))
	assert.Equal(t, 4321, timeout)
}

func TestUsageCacheFillReportsDeletedSession(t *testing.T) {
	require := require.New(t)

	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	version := snapshot.Versions[0]

	// Deletion is now observed by the fill's own archive read transaction, so
	// the delete has to land before extraction rather than after it.
	var deleteErr error
	cache.fill.observer = usageFillObserver{
		beforeExtract: func([]usageSourceVersion) {
			_, deleteErr = database.getWriter().Exec(
				`DELETE FROM sessions WHERE id = ?`, version.SessionID,
			)
		},
	}
	results, err := cache.fill.Ensure(t.Context(), []usageSourceVersion{version}, 0)
	require.NoError(err)
	require.NoError(deleteErr)
	assert.True(t, results[version.SessionID].Deleted)
}

// A source change that lands after extraction used to force a retry and, when
// it kept happening, fail the fill. The facts now come from a single archive
// read transaction, so the fill installs that snapshot and reports the version
// it read; the later write is picked up by its own notification fill.
func TestUsageCacheFillInstallsExtractedSnapshotDespiteLaterWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
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
		t.Context(), []usageSourceVersion{version}, 0,
	)
	require.NoError(err)
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(stored.(error))
	}
	assert.Equal(int32(1), mutations.Load(), "fill extracted more than once")
	assert.False(results[version.SessionID].Deleted)
	assert.Equal(version, results[version.SessionID].source)
}

// A write that lands after the request's archive snapshot no longer forces a
// recapture. The answer is exact as of the snapshot the request opened with,
// which is the intended bounded staleness: the session's own notification fill
// makes the new state visible to the next request.
func TestDailyUsageAnswersFromRequestSnapshot(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "moving", "keep", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(database.InsertMessages([]Message{{
		SessionID: "moving", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
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
	daily, err := database.GetDailyUsage(t.Context(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
		Project: "keep", SkipSessionCounts: true,
	})
	require.NoError(err)
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(stored.(error))
	}
	assert.Equal(t, 1, daily.Totals.InputTokens)
}

// A source fingerprint that keeps moving used to exhaust the fill's retry
// budget and fail. One archive read transaction is now always enough, so the
// fill extracts once and succeeds no matter how often the session is written.
func TestUsageCacheFillCompletesUnderContinuousWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
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
	results, err := cache.fill.Ensure(
		t.Context(), []usageSourceVersion{version}, 0)
	require.NoError(err)
	assert.Equal(int32(1), mutations.Load())
	assert.False(results[version.SessionID].Deleted)
}

func TestUsageCacheFillTwoHandlesRaceIdempotently(t *testing.T) {
	require := require.New(t)

	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(err)
	first, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	secondManager := newUsageCacheManager(database.path)
	secondManager.attachArchive(database)
	t.Cleanup(func() { require.NoError(secondManager.Close()) })
	second, err := secondManager.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)

	errorsCh := make(chan error, 2)
	for _, cache := range []*usageCache{first, second} {
		go func(cache *usageCache) {
			_, fillErr := cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
			errorsCh <- fillErr
		}(cache)
	}
	require.NoError(<-errorsCh)
	require.NoError(<-errorsCh)
	assert.Equal(t, len(snapshot.Versions),
		usageCacheCount(t, first, "usage_cached_sessions"))
}

func TestUsageCacheFillTokenCoverageRepairIsByteEquivalent(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "repair", "project", func(s *Session) {
		s.StartedAt = &started
	})
	require.NoError(database.InsertMessages([]Message{{
		SessionID: "repair", Ordinal: 0, Role: "assistant", Model: "model",
		Timestamp:  "2026-08-10T09:00:00Z",
		TokenUsage: json.RawMessage(`{"input_tokens":2,"output_tokens":3}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	_, err = cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(err)
	before := dumpCachedFacts(t, cache, "repair")

	_, err = database.getWriter().Exec(`
		UPDATE messages SET has_context_tokens = 0, has_output_tokens = 0
		WHERE session_id = 'repair';
		DELETE FROM stats WHERE key = ?`, tokenCoverageRepairStatsKey)
	require.NoError(err)
	database.mu.Lock()
	require.NoError(database.backfillTokenCoverageFlagsLocked(database.getWriter()))
	require.NoError(database.markTokenCoverageRepairDoneLocked(database.getWriter()))
	database.mu.Unlock()
	afterRepair, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(err)
	_, err = cache.fill.Ensure(t.Context(), afterRepair.Versions, 0)
	require.NoError(err)
	assert.Equal(t, before, dumpCachedFacts(t, cache, "repair"))
}

func TestUsageCursorFactsResumeAtRequestedHighWater(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := usageCandidateFixture(t)
	require.NoError(database.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt: "2026-08-11T00:00:00Z", Model: "cursor-2",
		InputTokens: 4, DedupKey: "cursor-2",
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)

	_, err = cache.fill.Ensure(t.Context(), nil, 1)
	require.NoError(err)
	assert.Equal(1, usageCacheCount(t, cache, "cursor_usage_facts"))
	_, err = cache.fill.Ensure(t.Context(), nil, snapshot.CursorHighWater)
	require.NoError(err)
	assert.Equal(2, usageCacheCount(t, cache, "cursor_usage_facts"))
}

func TestUsageCursorFactsRetainBoundedBatchProgress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	const batchSize = usageCursorCopyBatchSize
	events := make([]CursorUsageEvent, 0, batchSize+1)
	for index := range batchSize + 1 {
		events = append(events, CursorUsageEvent{
			OccurredAt: "2026-08-11T00:00:00Z", Model: "cursor-model",
			InputTokens: 1, DedupKey: fmt.Sprintf("cursor-batch-%04d", index),
		})
	}
	require.NoError(database.InsertCursorUsageEvents(events))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	_, err = cache.db.ExecContext(t.Context(), fmt.Sprintf(`
		CREATE TRIGGER reject_late_cursor_fact
		BEFORE INSERT ON cursor_usage_facts
		WHEN NEW.source_id > %d
		BEGIN
			SELECT RAISE(ABORT, 'stop after one bounded batch');
		END`, batchSize))
	require.NoError(err)

	_, err = cache.fill.Ensure(t.Context(), nil, snapshot.CursorHighWater)
	require.Error(err)
	assert.Equal(batchSize, usageCacheCount(t, cache, "cursor_usage_facts"))
	assert.Equal(strconv.Itoa(batchSize), readUsageCacheMetadata(
		t, cache.db)[usageCacheMetadataCursorHighWaterMark])

	_, err = cache.db.ExecContext(t.Context(), `DROP TRIGGER reject_late_cursor_fact`)
	require.NoError(err)
	_, err = cache.fill.Ensure(t.Context(), nil, snapshot.CursorHighWater)
	require.NoError(err)
	assert.Equal(batchSize+1, usageCacheCount(t, cache, "cursor_usage_facts"))
}

func TestUsageCursorFactsRejectChangedArchiveGeneration(t *testing.T) {
	require := require.New(t)

	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	_, err = database.getWriter().Exec(`
		UPDATE archive_metadata SET value = 'replacement-database'
		WHERE key = ?`, archiveMetadataDatabaseIDKey)
	require.NoError(err)
	_, err = cache.fill.Ensure(t.Context(), nil, snapshot.CursorHighWater)
	require.ErrorIs(err, errUsageCacheSourceChanged)
	assert.Zero(t, usageCacheCount(t, cache, "cursor_usage_facts"))
}

func TestUsageCursorFactsNormalizeRequestCounters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	require.NoError(database.InsertCursorUsageEvents([]CursorUsageEvent{{
		OccurredAt: "2026-08-11T00:00:00Z", Model: "cursor-model",
		InputTokens: -1, OutputTokens: usagefacts.MaxPlausibleTokens + 1,
		DedupKey: "cursor-normalized",
	}}))
	daily, err := database.GetDailyUsage(t.Context(), UsageFilter{})
	require.NoError(err)
	assert.Zero(daily.Totals.InputTokens)
	assert.Equal(usagefacts.MaxPlausibleTokens, daily.Totals.OutputTokens)
}

func TestUsageCacheNotificationIsNonblocking(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
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
	require := require.New(t)

	database := testDB(t)
	for _, id := range []string{"notify-a", "notify-b"} {
		insertSession(t, database, id, "project")
		require.NoError(database.InsertMessages([]Message{{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Timestamp: "2026-08-10T09:00:00Z", Model: "model",
			TokenUsage: json.RawMessage(`{"input_tokens":1}`),
		}}))
	}
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	_, err = cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(err)

	batch := make(chan []usageSourceVersion, 1)
	cache.fill.observer = usageFillObserver{
		beforeExtract: func(versions []usageSourceVersion) { batch <- versions },
	}
	_, err = database.getWriter().Exec(`UPDATE sessions
		SET transcript_revision = '2'
		WHERE id IN ('notify-a', 'notify-b')`)
	require.NoError(err)
	database.usageCache.NotifySessions([]string{"notify-a"})
	database.usageCache.NotifySessions([]string{"notify-a"})
	database.usageCache.NotifySessions([]string{"notify-b"})

	select {
	case versions := <-batch:
		require.Len(versions, 2)
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
	require := require.New(t)

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
	require.NoError(database.InsertMessages([]Message{initial}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(err)
	_, err = cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(err)

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
	require.NoError(database.ReplaceSessionMessages("notify", []Message{updated}))
	select {
	case got := <-observed:
		assert.JSONEq(t, `{"input_tokens":9}`, got)
	case <-time.After(30 * time.Second):
		t.Fatal("committed message write did not notify the usage cache")
	}

	duplicate := updated
	duplicate.TokenUsage = json.RawMessage(`{"input_tokens":10}`)
	require.Error(database.InsertMessages([]Message{duplicate}))
	select {
	case value := <-observed:
		t.Fatalf("rolled-back message write emitted usage notification: %s", value)
	case <-time.After(100 * time.Millisecond):
	}
}

func usageCacheCount(t *testing.T, cache *usageCache, table string) int {
	t.Helper()
	var count int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&count))
	return count
}

func dumpCachedFacts(t *testing.T, cache *usageCache, sessionID string) [][]any {
	t.Helper()
	rows, err := cache.db.QueryContext(t.Context(), `
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
