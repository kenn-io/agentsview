//go:build fts5

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/usagefacts"
)

func TestBulkReadEntryPointsWaitInLane(t *testing.T) {
	t.Run("saturated lane", func(t *testing.T) { testBulkReadEntryPoints(t, true) })
	t.Run("one reader connection", func(t *testing.T) { testBulkReadEntryPoints(t, false) })
}

func testBulkReadEntryPoints(t *testing.T, blocked bool) {
	t.Helper()
	database := usageCandidateFixture(t)
	message := asstMsgAt("inside-message", 1, "skill call", "2026-08-10T10:01:00Z")
	message.HasToolUse = true
	message.ToolCalls = []ToolCall{
		{SessionID: "inside-message", ToolName: "Skill", Category: "Skill", SkillName: "review-code"},
		{SessionID: "inside-message", ToolName: "Edit", Category: "Edit", FilePath: "example.go", CallIndex: 1},
	}
	insertMessages(t, database, message)
	database.rawReader().SetMaxOpenConns(1)
	filter := AnalyticsFilter{From: "2026-08-01", To: "2026-08-20", Timezone: "UTC"}
	snapshot, err := database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	defer snapshot.Close()
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-08-10", Timezone: "UTC",
	}, time.Now())
	require.NoError(t, err)
	if blocked {
		for range bulkReadSlots {
			release, err := database.acquireBulkRead(t.Context())
			require.NoError(t, err)
			defer release()
		}
	}
	for _, tc := range []struct {
		name        string
		read        func(context.Context) error
		wantError   error
		blockedOnly bool
	}{
		{"summary", func(ctx context.Context) error {
			_, err := database.GetAnalyticsSummary(ctx, filter)
			return err
		}, nil, false},
		{"activity", func(ctx context.Context) error {
			_, err := database.GetAnalyticsActivity(ctx, filter, "day")
			return err
		}, nil, false},
		{"heatmap", func(ctx context.Context) error {
			_, err := database.GetAnalyticsHeatmap(ctx, filter, "messages")
			return err
		}, nil, false},
		{"projects", func(ctx context.Context) error {
			_, err := database.GetAnalyticsProjects(ctx, filter)
			return err
		}, nil, false},
		{"project inventory", func(ctx context.Context) error {
			_, err := database.GetProjectInventory(ctx, ProjectDateFilter{})
			return err
		}, nil, false},
		{"hour of week", func(ctx context.Context) error {
			_, err := database.GetAnalyticsHourOfWeek(ctx, filter)
			return err
		}, nil, false},
		{"session shape", func(ctx context.Context) error {
			_, err := database.GetAnalyticsSessionShape(ctx, filter)
			return err
		}, nil, false},
		{"tools", func(ctx context.Context) error {
			_, err := database.GetAnalyticsTools(ctx, filter)
			return err
		}, nil, false},
		{"skills", func(ctx context.Context) error {
			_, err := database.GetAnalyticsSkills(ctx, filter, "week")
			return err
		}, nil, false},
		{"velocity", func(ctx context.Context) error {
			_, err := database.GetAnalyticsVelocity(ctx, filter)
			return err
		}, nil, false},
		{"signals", func(ctx context.Context) error {
			_, err := database.GetAnalyticsSignals(ctx, filter)
			return err
		}, nil, false},
		{"signal sessions", func(ctx context.Context) error {
			_, err := database.GetAnalyticsSignalSessions(ctx, filter, "outcome_completed", 10)
			return err
		}, nil, false},
		{"tool signal sessions", func(ctx context.Context) error {
			_, err := database.GetAnalyticsSignalSessions(ctx, filter, "tool_empty_rate", 10)
			return err
		}, nil, false},
		{"top sessions", func(ctx context.Context) error {
			_, err := database.GetAnalyticsTopSessions(ctx, filter, "messages")
			return err
		}, nil, false},
		{"session stats", func(ctx context.Context) error {
			_, err := database.GetSessionStats(ctx, StatsFilter{Since: "2026-08-01", Until: "2026-08-21", Timezone: "UTC"})
			return err
		}, nil, false},
		{"archive regex search", func(ctx context.Context) error {
			_, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: `[a-z]+ call`, Mode: "regex"})
			return err
		}, nil, false},
		{"prefixed regex search", func(ctx context.Context) error {
			_, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: "skill", Mode: "regex"})
			return err
		}, nil, false},
		{"substring search", func(ctx context.Context) error {
			_, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: "skill", Mode: "substring"})
			return err
		}, nil, false},
		{"terms search", func(ctx context.Context) error {
			_, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: "question", Mode: "terms", IncludeOneShot: true})
			return err
		}, nil, false},
		{"embeddable units", func(ctx context.Context) error {
			var units []EmbeddableUnit
			_, err := database.ScanEmbeddableUnits(ctx, "", false, func(unit EmbeddableUnit) error {
				units = append(units, unit)
				return nil
			})
			if blocked {
				assert.Empty(t, units)
			}
			return err
		}, nil, false},
		{"recall embedding units", func(ctx context.Context) error {
			var units []EmbeddableUnit
			_, err := database.ScanRecallEmbeddingUnits(ctx, "", func(unit EmbeddableUnit) error {
				units = append(units, unit)
				return nil
			})
			if blocked {
				assert.Empty(t, units)
			}
			return err
		}, nil, false},
		{"recent edits", func(ctx context.Context) error {
			_, err := database.RecentEdits(ctx, RecentEditsParams{})
			return err
		}, nil, false},
		{"trends", func(ctx context.Context) error {
			_, err := database.GetTrendsTerms(ctx, filter, []TrendTermInput{{Term: "test"}}, "week")
			return err
		}, nil, false},
		{"activity artifacts", func(ctx context.Context) error {
			_, err := database.BuildActivityReportArtifacts(ctx, filter, q, nil)
			return err
		}, nil, true},
		{"usage capture", func(ctx context.Context) error {
			captured, err := database.captureUsageQuery(ctx, UsageFilter{}, usageQueryKindToken)
			if err != nil {
				return err
			}
			defer captured.Close()
			return nil
		}, nil, true},
		{"usage source recheck", func(ctx context.Context) error {
			_, err := cache.fill.recheckSourceVersions(ctx, snapshot.Versions)
			return err
		}, nil, true},
		{"usage backfill activity", func(ctx context.Context) error {
			_, err := database.usageBackfillActivity(ctx)
			return err
		}, nil, false},
		{"usage extraction", func(ctx context.Context) error {
			spool, versions, err := cache.fill.extractSessions(ctx, snapshot.Versions)
			if spool != nil {
				defer spool.Close()
			}
			if blocked {
				assert.Nil(t, spool)
				assert.Nil(t, versions)
			}
			return err
		}, nil, false},
		{"Cursor batch", func(ctx context.Context) error {
			_, _, err := cache.fill.readCursorBatch(ctx, 0, snapshot.CursorHighWater)
			return err
		}, nil, false},
		{"unsupported signal", func(ctx context.Context) error {
			_, err := database.GetAnalyticsSignalSessions(ctx, filter, "unsupported", 10)
			return err
		}, ErrUnsupportedAnalyticsSignal, false},
		{"invalid stats timezone", func(ctx context.Context) error {
			_, err := database.GetSessionStats(ctx, StatsFilter{Timezone: "invalid"})
			return err
		}, &StatsInputError{}, false},
		{"invalid stats window", func(ctx context.Context) error {
			_, err := database.GetSessionStats(ctx, StatsFilter{Since: "2026-08-11", Until: "2026-08-10"})
			return err
		}, &StatsInputError{}, false},
		{"invalid regex", func(ctx context.Context) error {
			_, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: "[", Mode: "regex"})
			return err
		}, &SearchInputError{}, false},
		{"invalid terms scope", func(ctx context.Context) error {
			_, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: "question", Mode: "terms", Scope: "invalid"})
			return err
		}, &SearchInputError{}, false},
		{"invalid substring source", func(ctx context.Context) error {
			_, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: "skill", Mode: "substring", Sources: []string{"invalid"}})
			return err
		}, &SearchInputError{}, false},
	} {
		if !blocked && tc.wantError != nil {
			continue
		}
		if !blocked && tc.blockedOnly {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			budget := 30 * time.Second
			if blocked && tc.wantError == nil {
				budget = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()
			err := tc.read(ctx)
			if tc.wantError != nil {
				if statsError, ok := errors.AsType[*StatsInputError](tc.wantError); ok {
					require.ErrorAs(t, err, &statsError)
				} else if searchError, ok := errors.AsType[*SearchInputError](tc.wantError); ok {
					require.ErrorAs(t, err, &searchError)
				} else {
					require.ErrorIs(t, err, tc.wantError)
				}
				require.NoError(t, ctx.Err())
			} else if blocked {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			} else {
				require.NoError(t, err, "bulk holder must complete with one reader connection")
			}
			if blocked {
				assert.Zero(t, database.rawReader().Stats().InUse)
			}
		})
	}
	if !blocked {
		return
	}
	for _, tc := range []struct {
		mode, sessionID, pattern, snippet string
	}{
		{"substring", "inside-message", "skill", "skill call"},
		{"terms", "user-only", "question", "question"},
		{"regex", "inside-message", "skill", "skill call"},
	} {
		t.Run("session-scoped "+tc.mode+" search bypasses lane", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			page, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: tc.pattern, Mode: tc.mode, SessionID: tc.sessionID, Sources: []string{"messages"}, IncludeOneShot: true})
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			assert.Equal(t, tc.sessionID, page.Matches[0].SessionID)
			assert.Equal(t, tc.snippet, page.Matches[0].Snippet)
			require.NoError(t, ctx.Err())
			assert.Zero(t, database.rawReader().Stats().InUse)
		})
	}
	t.Run("fts search bypasses lane", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		page, err := database.SearchContent(ctx, ContentSearchFilter{Pattern: "skill", Mode: "fts", Sources: []string{"messages"}})
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		assert.Equal(t, "inside-message", page.Matches[0].SessionID)
		require.NoError(t, ctx.Err())
		assert.Zero(t, database.rawReader().Stats().InUse)
	})
}

func TestUsageCacheFillInstallsCompleteSessionFacts(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "fill-session", "project", func(s *Session) {
		s.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages(t.Context(), []Message{
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
	require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), "fill-session", []UsageEvent{{
		Source: "session", Model: "event-model", InputTokens: 5,
		OccurredAt: "2026-06-01T09:00:00Z", DedupKey: "event-1",
	}}))

	snapshot, err := database.captureUsageQuery(t.Context(), UsageFilter{
		From: "2026-08-10", To: "2026-08-10", Timezone: "UTC",
	}, usageQueryKindActivity)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	results, err := cache.fill.Ensure(
		t.Context(), snapshot.Versions, snapshot.CursorHighWater,
	)
	require.NoError(t, err)
	assert.False(t, results["fill-session"].Deleted)
	assert.Positive(t, results["fill-session"].InstallRevision)

	rows, err := cache.db.QueryContext(t.Context(), `
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
	conn, err := database.getReader().Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `
		CREATE TEMP TABLE usage_fill_sessions(
			session_id TEXT PRIMARY KEY
		) WITHOUT ROWID;
		INSERT INTO usage_fill_sessions VALUES ('inside-message')`)
	require.NoError(t, err)

	plan := func(query string) string {
		t.Helper()
		rows, queryErr := conn.QueryContext(
			t.Context(), `EXPLAIN QUERY PLAN `+query,
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

type countingUsageCacheStatementRunner struct {
	usageCacheStatementRunner
	queryArgCounts []int
}

func (r *countingUsageCacheStatementRunner) QueryContext(
	ctx context.Context, query string, args ...any,
) (*sql.Rows, error) {
	r.queryArgCounts = append(r.queryArgCounts, len(args))
	return r.usageCacheStatementRunner.QueryContext(ctx, query, args...)
}

func TestReadUsageCachedSessionsCrossesChunkBoundary(t *testing.T) {
	database := testDB(t)
	snapshot, err := database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	tx, err := cache.db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer tx.Rollback()
	want := make(map[string]usageFillResult)
	ids := make([]string, 0, maxSQLVars+5)
	for i := range maxSQLVars + 2 {
		id := fmt.Sprintf("session-%d", i)
		result := usageFillResult{
			InstallRevision: int64(i + 1),
			source: usageSourceVersion{
				SessionID: id, SyncMarker: fmt.Sprintf("sync-%d", i),
				TranscriptRevision:    fmt.Sprintf("revision-%d", i),
				UsageEventFingerprint: fmt.Sprintf("events-%d", i),
			},
		}
		_, err := tx.ExecContext(t.Context(), `INSERT INTO usage_cached_sessions(
			session_id, source_sync_marker, source_transcript_rev,
			usage_event_fingerprint, install_revision) VALUES (?, ?, ?, ?, ?)`,
			id, result.source.SyncMarker, result.source.TranscriptRevision,
			result.source.UsageEventFingerprint, result.InstallRevision)
		require.NoError(t, err)
		ids = append(ids, id)
		want[id] = result
	}
	require.NoError(t, tx.Commit())
	ids = append(ids, "absent-1", "absent-2", "absent-3")
	runner := &countingUsageCacheStatementRunner{usageCacheStatementRunner: cache.db}
	empty, err := readUsageCachedSessions(t.Context(), runner, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
	assert.Empty(t, runner.queryArgCounts)
	got, err := readUsageCachedSessions(t.Context(), runner, ids)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, []int{maxSQLVars, 5}, runner.queryArgCounts)
}

func TestUsageFillEnsureRefillsOnlyChangedSessions(t *testing.T) {
	database := testDB(t)
	database.SetUsageCacheNotificationWork(func() (func(), bool) {
		return nil, false
	})
	for _, id := range []string{"changed", "unchanged"} {
		insertSession(t, database, id, "project")
		insertMessages(t, database, asstMsgAt(id, 0, "answer", "2026-08-10T10:00:00Z"))
	}
	snapshot, err := database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	require.Len(t, snapshot.Versions, 2)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	first, err := cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(t, err)
	insertMessages(t, database, asstMsgAt("changed", 1, "more", "2026-08-10T10:01:00Z"))
	snapshot, err = database.captureUsageQuery(t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	var extracted []string
	cache.fill.observer.beforeExtract = func(versions []usageSourceVersion) {
		extracted = append(extracted, usageSourceVersionIDs(versions)...)
	}
	second, err := cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"changed"}, extracted)
	assert.Equal(t, first["unchanged"], second["unchanged"])
	assert.Greater(t, second["changed"].InstallRevision, first["changed"].InstallRevision)
}

func TestUsageFillCoordinatorSharesDetachedSessionWork(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
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
	require.ErrorIs(t, <-firstDone, context.Canceled)
	require.NoError(t, <-secondDone)
	assert.Equal(t, int32(1), extractions.Load())
}

func TestUsageFillCoordinatorDoesNotJoinOlderSourceVersion(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
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
			t.Context(), []usageSourceVersion{oldVersion}, 0)
		oldDone <- fillErr
	}()
	<-oldStarted
	_, err = database.getWriter().Exec(t.Context(), `
		UPDATE sessions SET transcript_revision = 'newer' WHERE id = ?`,
		oldVersion.SessionID)
	require.NoError(t, err)
	current, err := cache.fill.recheckSourceVersions(
		t.Context(), []usageSourceVersion{{SessionID: oldVersion.SessionID}})
	require.NoError(t, err)
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
		require.Fail(t, "newer source version joined an older in-flight fill")
	}
	close(releaseOld)
	require.NoError(t, <-newDone)
	// The requested version still keys the single-flight call, so the newer
	// version cannot join the older one's in-flight fill. The older caller
	// itself no longer fails: its extraction reads whatever the archive holds
	// when its own read transaction opens, and it reports that version back.
	require.NoError(t, <-oldDone)
}

func TestUsageFillOlderExtractionCannotReplaceNewerCachedFacts(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	var oldVersion usageSourceVersion
	for _, version := range snapshot.Versions {
		if version.SessionID == "inside-message" {
			oldVersion = version
			break
		}
	}
	require.Equal(t, "inside-message", oldVersion.SessionID)
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
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE messages SET token_usage = '{"input_tokens":9}'
		WHERE session_id = ?`, oldVersion.SessionID)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE sessions SET transcript_revision = 'newer'
		WHERE id = ?`, oldVersion.SessionID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	current, err := cache.fill.recheckSourceVersions(
		t.Context(), []usageSourceVersion{{SessionID: oldVersion.SessionID}})
	require.NoError(t, err)
	newVersion := current[oldVersion.SessionID]
	newResults, err := cache.fill.Ensure(
		t.Context(), []usageSourceVersion{newVersion}, 0)
	require.NoError(t, err)
	close(releaseOld)
	oldOutcome := <-oldDone
	require.NoError(t, oldOutcome.err)

	assert.Equal(t, newVersion, newResults[oldVersion.SessionID].source)
	assert.Equal(t, newVersion, oldOutcome.results[oldVersion.SessionID].source)
	assert.Equal(t, int32(2), oldExtractions.Load(),
		"the older fill should re-extract after losing the cache race")
	var inputTokens int64
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT f.input_tokens
		FROM usage_facts f JOIN usage_cached_sessions s
		  ON s.id = f.cached_session_id
		WHERE s.session_id = ?`, oldVersion.SessionID).Scan(&inputTokens))
	assert.Equal(t, int64(9), inputTokens)
}

func TestUsageFillRecheckRestoresReaderBusyTimeout(t *testing.T) {
	database := usageCandidateFixture(t)
	database.reader.Load().SetMaxOpenConns(1)
	conn, err := database.getReader().Conn(t.Context())
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA busy_timeout=4321`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.fill.recheckSourceVersions(t.Context(), snapshot.Versions[:1])
	require.NoError(t, err)
	var timeout int
	require.NoError(t, database.getReader().QueryRow(t.Context(),
		`PRAGMA busy_timeout`).Scan(&timeout))
	assert.Equal(t, 4321, timeout)
}

func TestUsageCacheFillReportsDeletedSession(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	version := snapshot.Versions[0]

	// Deletion is now observed by the fill's own archive read transaction, so
	// the delete has to land before extraction rather than after it.
	var deleteErr error
	cache.fill.observer = usageFillObserver{
		beforeExtract: func([]usageSourceVersion) {
			_, deleteErr = database.getWriter().Exec(t.Context(),
				`DELETE FROM sessions WHERE id = ?`, version.SessionID,
			)
		},
	}
	results, err := cache.fill.Ensure(t.Context(), []usageSourceVersion{version}, 0)
	require.NoError(t, err)
	require.NoError(t, deleteErr)
	assert.True(t, results[version.SessionID].Deleted)
}

// A source change that lands after extraction used to force a retry and, when
// it kept happening, fail the fill. The facts now come from a single archive
// read transaction, so the fill installs that snapshot and reports the version
// it read; the later write is picked up by its own notification fill.
func TestUsageCacheFillInstallsExtractedSnapshotDespiteLaterWrite(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	version := snapshot.Versions[0]
	var mutations atomic.Int32
	var mutationErr atomic.Value
	cache.fill.observer = usageFillObserver{
		afterExtract: func([]usageSourceVersion) {
			if mutations.Add(1) != 1 {
				return
			}
			_, updateErr := database.getWriter().Exec(t.Context(), `
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
	require.NoError(t, err)
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(t, stored.(error))
	}
	assert.Equal(t, int32(1), mutations.Load(), "fill extracted more than once")
	assert.False(t, results[version.SessionID].Deleted)
	assert.Equal(t, version, results[version.SessionID].source)
}

// A write that lands after the request's archive snapshot no longer forces a
// recapture. The answer is exact as of the snapshot the request opened with,
// which is the intended bounded staleness: the session's own notification fill
// makes the new state visible to the next request.
func TestDailyUsageAnswersFromRequestSnapshot(t *testing.T) {
	database := testDB(t)
	started := "2026-08-10T08:00:00Z"
	insertSession(t, database, "moving", "keep", func(session *Session) {
		session.StartedAt = &started
	})
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "moving", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-08-10T09:00:00Z", Model: "model",
		TokenUsage: json.RawMessage(`{"input_tokens":1}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	var mutations atomic.Int32
	var mutationErr atomic.Value
	cache.fill.observer.afterExtract = func([]usageSourceVersion) {
		if mutations.Add(1) != 1 {
			return
		}
		_, updateErr := database.getWriter().Exec(t.Context(), `
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
	require.NoError(t, err)
	if stored := mutationErr.Load(); stored != nil {
		require.NoError(t, stored.(error))
	}
	assert.Equal(t, 1, daily.Totals.InputTokens)
}

// A source fingerprint that keeps moving used to exhaust the fill's retry
// budget and fail. One archive read transaction is now always enough, so the
// fill extracts once and succeeds no matter how often the session is written.
func TestUsageCacheFillCompletesUnderContinuousWrites(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	version := snapshot.Versions[0]
	var mutations atomic.Int32
	cache.fill.observer = usageFillObserver{
		afterExtract: func([]usageSourceVersion) {
			n := mutations.Add(1)
			_, _ = database.getWriter().Exec(t.Context(), `
				UPDATE sessions SET transcript_revision = ? WHERE id = ?`,
				fmt.Sprintf("moving-%d", n), version.SessionID)
		},
	}
	results, err := cache.fill.Ensure(
		t.Context(), []usageSourceVersion{version}, 0)
	require.NoError(t, err)
	assert.Equal(t, int32(1), mutations.Load())
	assert.False(t, results[version.SessionID].Deleted)
}

func TestUsageCacheFillTwoHandlesRaceIdempotently(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	first, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	secondManager := newUsageCacheManager(database.path)
	secondManager.attachArchive(database)
	t.Cleanup(func() { require.NoError(t, secondManager.Close()) })
	second, err := secondManager.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)

	errorsCh := make(chan error, 2)
	for _, cache := range []*usageCache{first, second} {
		go func(cache *usageCache) {
			_, fillErr := cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
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
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{
		SessionID: "repair", Ordinal: 0, Role: "assistant", Model: "model",
		Timestamp:  "2026-08-10T09:00:00Z",
		TokenUsage: json.RawMessage(`{"input_tokens":2,"output_tokens":3}`),
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(t, err)
	before := dumpCachedFacts(t, cache, "repair")

	_, err = database.getWriter().Exec(t.Context(), `
		UPDATE messages SET has_context_tokens = 0, has_output_tokens = 0
		WHERE session_id = 'repair';
		DELETE FROM stats WHERE key = ?`, tokenCoverageRepairStatsKey)
	require.NoError(t, err)
	database.mu.Lock()
	require.NoError(t, database.backfillTokenCoverageFlagsLocked(t.Context(), database.getWriter()))
	require.NoError(t, database.markTokenCoverageRepairDoneLocked(t.Context(), database.getWriter()))
	database.mu.Unlock()
	afterRepair, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(t.Context(), afterRepair.Versions, 0)
	require.NoError(t, err)
	assert.Equal(t, before, dumpCachedFacts(t, cache, "repair"))
}

func TestUsageCursorFactsResumeAtRequestedHighWater(t *testing.T) {
	database := usageCandidateFixture(t)
	require.NoError(t, database.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{
		OccurredAt: "2026-08-11T00:00:00Z", Model: "cursor-2",
		InputTokens: 4, DedupKey: "cursor-2",
	}}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)

	_, err = cache.fill.Ensure(t.Context(), nil, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, usageCacheCount(t, cache, "cursor_usage_facts"))
	_, err = cache.fill.Ensure(t.Context(), nil, snapshot.CursorHighWater)
	require.NoError(t, err)
	assert.Equal(t, 2, usageCacheCount(t, cache, "cursor_usage_facts"))
}

func usageCursorBatchFailureFixture(t *testing.T, database *DB) (usageQuerySnapshot, *usageCache) {
	t.Helper()
	const batchSize = usageCursorCopyBatchSize
	events := make([]CursorUsageEvent, 0, batchSize+1)
	for index := range batchSize + 1 {
		events = append(events, CursorUsageEvent{
			OccurredAt: "2026-08-11T00:00:00Z", Model: "cursor-model",
			InputTokens: 1, DedupKey: fmt.Sprintf("cursor-batch-%04d", index),
		})
	}
	require.NoError(t, database.InsertCursorUsageEvents(t.Context(), events))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.db.ExecContext(t.Context(), fmt.Sprintf(`
		CREATE TRIGGER reject_late_cursor_fact
		BEFORE INSERT ON cursor_usage_facts
		WHEN NEW.source_id > %d
		BEGIN
			SELECT RAISE(ABORT, 'stop after one bounded batch');
		END`, batchSize))
	require.NoError(t, err)

	return snapshot, cache
}

func TestUsageCursorFactsRetainBoundedBatchProgress(t *testing.T) {
	database := testDB(t)
	snapshot, cache := usageCursorBatchFailureFixture(t, database)
	const batchSize = usageCursorCopyBatchSize

	_, err := cache.fill.Ensure(t.Context(), nil, snapshot.CursorHighWater)
	require.Error(t, err)
	assert.Equal(t, batchSize, usageCacheCount(t, cache, "cursor_usage_facts"))
	assert.Equal(t, strconv.Itoa(batchSize), readUsageCacheMetadata(
		t, cache.db)[usageCacheMetadataCursorHighWaterMark])

	_, err = cache.db.ExecContext(t.Context(), `DROP TRIGGER reject_late_cursor_fact`)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(t.Context(), nil, snapshot.CursorHighWater)
	require.NoError(t, err)
	assert.Equal(t, batchSize+1, usageCacheCount(t, cache, "cursor_usage_facts"))
}

func TestUsageCursorFactsRejectChangedArchiveGeneration(t *testing.T) {
	database := usageCandidateFixture(t)
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = database.getWriter().Exec(t.Context(), `
		UPDATE archive_metadata SET value = 'replacement-database'
		WHERE key = ?`, archiveMetadataDatabaseIDKey)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(t.Context(), nil, snapshot.CursorHighWater)
	require.ErrorIs(t, err, errUsageCacheSourceChanged)
	assert.Zero(t, usageCacheCount(t, cache, "cursor_usage_facts"))
}

func TestUsageCursorFactsNormalizeRequestCounters(t *testing.T) {
	database := testDB(t)
	require.NoError(t, database.InsertCursorUsageEvents(t.Context(), []CursorUsageEvent{{
		OccurredAt: "2026-08-11T00:00:00Z", Model: "cursor-model",
		InputTokens: -1, OutputTokens: usagefacts.MaxPlausibleTokens + 1,
		DedupKey: "cursor-normalized",
	}}))
	daily, err := database.GetDailyUsage(t.Context(), UsageFilter{})
	require.NoError(t, err)
	assert.Zero(t, daily.Totals.InputTokens)
	assert.Equal(t, usagefacts.MaxPlausibleTokens, daily.Totals.OutputTokens)
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
		require.Fail(t, "usage cache notification blocked on fill work")
	}
	close(blocked)
}

func TestUsageCacheNotificationsCoalesceChangedSessions(t *testing.T) {
	database := testDB(t)
	for _, id := range []string{"notify-a", "notify-b"} {
		insertSession(t, database, id, "project")
		require.NoError(t, database.InsertMessages(t.Context(), []Message{{
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
	_, err = database.getWriter().Exec(t.Context(), `UPDATE sessions
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
		require.Fail(t, "coalesced usage cache fill did not start")
	}
	select {
	case versions := <-batch:
		require.Failf(t, "duplicate notification started another fill", "%#v", versions)
	case <-time.After(3 * usageFillNotificationDebounce): //nolint:kennlint // absence check; the coalesced fill must not start a second batch
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
	require.NoError(t, database.InsertMessages(t.Context(), []Message{initial}))
	snapshot, err := database.captureUsageQuery(
		t.Context(), UsageFilter{}, usageQueryKindToken,
	)
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), snapshot.DatabaseID)
	require.NoError(t, err)
	_, err = cache.fill.Ensure(t.Context(), snapshot.Versions, 0)
	require.NoError(t, err)

	observed := make(chan string, 1)
	cache.fill.observer = usageFillObserver{
		beforeExtract: func([]usageSourceVersion) {
			var tokenUsage string
			queryErr := database.getReader().QueryRow(t.Context(), `
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
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), "notify", []Message{updated}))
	select {
	case got := <-observed:
		assert.JSONEq(t, `{"input_tokens":9}`, got)
	case <-time.After(30 * time.Second):
		require.Fail(t, "committed message write did not notify the usage cache")
	}

	duplicate := updated
	duplicate.TokenUsage = json.RawMessage(`{"input_tokens":10}`)
	require.Error(t, database.InsertMessages(t.Context(), []Message{duplicate}))
	select {
	case value := <-observed:
		require.Failf(t, "rolled-back message write emitted usage notification", "%s", value)
	case <-time.After(100 * time.Millisecond): //nolint:kennlint // absence check; the rolled-back write must never notify
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
