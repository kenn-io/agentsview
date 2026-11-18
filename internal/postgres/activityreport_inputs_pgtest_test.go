//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/money"
)

func openPGActivityInputStore(t *testing.T, target, schema, directory string) *Store {
	t.Helper()
	store, err := NewStore(t.Context(), target, schema, true)
	require.NoError(t, err)
	require.NoError(t, store.ConfigureUsageCache(t.Context(), directory, target, schema))
	return store
}

type activityInputReadCounter struct {
	storeExecutor
	mu                          sync.Mutex
	transcriptReads, usageReads [][]string
	cancel                      context.CancelFunc
}

type activityIdentityError struct {
	storeExecutor
	queries int
	denied  bool
}

func (c *activityIdentityError) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, "pg_control_system()") {
		c.queries++
		if c.denied {
			return c.storeExecutor.QueryRowContext(ctx, "SELECT activity_identity_denied()")
		}
		if c.queries == 1 {
			return c.storeExecutor.QueryRowContext(ctx, "SELECT 1 / 0")
		}
	}
	return c.storeExecutor.QueryRowContext(ctx, query, args...)
}

func (c *activityInputReadCounter) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	transcript := strings.HasPrefix(query, "SELECT session_id, ordinal, role, timestamp, model")
	usage := strings.Contains(query, "usage_events") && !strings.Contains(query, "usage_session_changes")
	if transcript || usage {
		c.mu.Lock()
		ids := append([]string(nil), args[1].([]string)...)
		if transcript {
			c.transcriptReads = append(c.transcriptReads, ids)
		}
		if usage {
			c.usageReads = append(c.usageReads, ids)
		}
		cancel := c.cancel
		c.mu.Unlock()
		if transcript && cancel != nil {
			cancel()
		}
	}
	return c.storeExecutor.QueryContext(ctx, query, args...)
}

func TestPGActivityReportInputReads(t *testing.T) {
	t.Parallel()
	local, syncer, store := newUsageJournalFixture(t)
	for _, id := range []string{"a", "b"} {
		pgKeptSession(t, local, id, "project-a", "2026-06-14")
	}
	pgKeptPush(t, syncer, false)
	counter := &activityInputReadCounter{storeExecutor: store.pg}
	store.pg = counter
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	q.Partial, q.EffectiveEnd = true, q.RangeEnd.Add(-time.Hour)
	before := pgKeptBuild(t, store, f, q, true)
	require.Len(t, counter.transcriptReads, 1)
	assert.ElementsMatch(t, []string{"a", "b"}, counter.transcriptReads[0])
	require.Len(t, counter.usageReads, 1)
	repeated := pgKeptBuild(t, store, f, q, true)
	requirePGKeptEqual(t, before, repeated)
	assert.Len(t, counter.transcriptReads, 1)
	assert.Len(t, counter.usageReads, 1)
	assert.Equal(t, 1000, repeated.Report.Totals.OutputTokens)
	assert.Len(t, repeated.Sessions, 2)
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "a", []db.Message{
		{SessionID: "a", Ordinal: 0, Role: "user", Content: "question", Timestamp: "2026-06-14T10:00:00Z"},
		{SessionID: "a", Ordinal: 1, Role: "assistant", Content: "answer", Timestamp: "2026-06-14T10:02:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":700}`)},
	}))
	pgKeptPush(t, syncer, false)
	changed := pgKeptBuild(t, store, f, q, true)
	require.Len(t, counter.transcriptReads, 2)
	assert.Equal(t, []string{"a"}, counter.transcriptReads[1])
	require.Len(t, counter.usageReads, 2)
	assert.Equal(t, []string{"a"}, counter.usageReads[1])
	assert.Equal(t, 1200, changed.Report.Totals.OutputTokens)
	store.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
	pgKeptBuild(t, store, f, q, true)
	assert.Len(t, counter.transcriptReads, 2)
	assert.Len(t, counter.usageReads, 2)
}

func TestPGActivityReportInputSharedBudget(t *testing.T) {
	t.Parallel()
	local, syncer, store := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	counter := &activityInputReadCounter{storeExecutor: store.pg}
	store.pg = counter
	f := db.AnalyticsFilter{Timezone: "UTC"}
	day := pgDayQuery(t, "2026-06-14", "UTC")
	day.Partial = true
	day.EffectiveEnd = time.Date(2026, 6, 14, 11, 0, 0, 0, time.UTC)
	month, err := activity.ResolveQuery(
		activity.QueryInput{Preset: "month", Date: "2026-06-14", Timezone: "UTC"},
		time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	month.Partial = true
	month.EffectiveEnd = day.EffectiveEnd
	version, err := store.activityKeptVersion(t.Context(), "")
	require.NoError(t, err)

	daySlot := db.PaddedUTCBound(day.RangeStart.UTC().Format(time.RFC3339), -14) + "|" +
		db.PaddedUTCBound(day.RangeEnd.UTC().Format(time.RFC3339), 14)
	monthSlot := db.PaddedUTCBound(month.RangeStart.UTC().Format(time.RFC3339), -14) + "|" +
		db.PaddedUTCBound(month.RangeEnd.UTC().Format(time.RFC3339), 14)
	wantDay := pgKeptBuild(t, store, f, day, true)
	wantMonth := pgKeptBuild(t, store, f, month, true)
	dayEntry, ok := store.runtimeState().activityInputs.Get(daySlot, version)
	require.True(t, ok)
	monthEntry, ok := store.runtimeState().activityInputs.Get(monthSlot, version)
	require.True(t, ok)
	dayFullBytes := pgActivityInputBytes(dayEntry[0])
	monthFullBytes := pgActivityInputBytes(monthEntry[0])
	dayRaw, monthRaw := dayEntry[0], monthEntry[0]
	dayRaw.priced, monthRaw.priced = nil, nil
	dayRawBytes, monthRawBytes := pgActivityInputBytes(dayRaw), pgActivityInputBytes(monthRaw)
	budget := dayRawBytes + monthRawBytes
	require.LessOrEqual(t, dayFullBytes, budget)
	require.LessOrEqual(t, monthFullBytes, budget)
	require.Greater(t, dayFullBytes+monthFullBytes, budget)

	store.runtimeState().activityInputs.MaxBytes = budget
	store.runtimeState().activityInputs.DeleteSlots(func(string) bool { return true })
	counter.mu.Lock()
	counter.transcriptReads, counter.usageReads = nil, nil
	counter.mu.Unlock()
	for range 2 {
		gotDay := pgKeptBuild(t, store, f, day, true)
		requirePGKeptEqual(t, wantDay, gotDay)
		gotMonth := pgKeptBuild(t, store, f, month, true)
		requirePGKeptEqual(t, wantMonth, gotMonth)
	}
	assert.Len(t, counter.usageReads, 2)
	var retained int64
	for _, slot := range []string{daySlot, monthSlot} {
		entries, ok := store.runtimeState().activityInputs.Get(slot, version)
		require.True(t, ok)
		require.Nil(t, entries[0].priced)
		retained += pgActivityInputBytes(entries[0])
	}
	assert.LessOrEqual(t, retained, budget)
	store.runtimeState().activityInputs.MaxBytes = dayRawBytes
	requirePGKeptEqual(t, wantDay, pgKeptBuild(t, store, f, day, true))
	entries, ok := store.runtimeState().activityInputs.Get(daySlot, version)
	require.True(t, ok)
	assert.Nil(t, entries[0].priced)
}

func TestPGActivityReportInputClock(t *testing.T) {
	t.Parallel()
	local, syncer, store := newUsageJournalFixture(t)
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: "open", Project: "project-a", Machine: "test-machine", Agent: "claude", StartedAt: new("2026-06-14T10:00:00Z"), MessageCount: 3}))
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "open", []db.Message{
		{SessionID: "open", Ordinal: 0, Role: "assistant", Content: "earlier", Timestamp: "2026-06-14T09:59:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":100}`)},
		{SessionID: "open", Ordinal: 1, Role: "user", Content: "question", Timestamp: "2026-06-14T10:00:00Z"},
		{SessionID: "open", Ordinal: 2, Role: "assistant", Content: "future answer", Timestamp: "2026-06-14T10:05:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":300}`)},
	}))
	pgKeptPush(t, syncer, false)
	counter := &activityInputReadCounter{storeExecutor: store.pg}
	store.pg = counter
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	q.Partial = true
	q.EffectiveEnd = q.RangeStart.Add(10*time.Hour + time.Minute)
	before := pgKeptBuild(t, store, f, q, true)
	assert.Equal(t, 100, before.Report.Totals.OutputTokens)
	version, err := store.activityKeptVersion(t.Context(), "")
	require.NoError(t, err)
	lower := db.PaddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upper := db.PaddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)
	entries, ok := store.runtimeState().activityInputs.Get(lower+"|"+upper, version)
	require.True(t, ok)
	saved := entries[0].priced
	require.NotNil(t, saved)
	q.EffectiveEnd = q.EffectiveEnd.Add(time.Minute)
	advanced := pgKeptBuild(t, store, f, q, true)
	entries, ok = store.runtimeState().activityInputs.Get(lower+"|"+upper, version)
	require.True(t, ok)
	assert.Same(t, saved, entries[0].priced)
	assert.Equal(t, 100, advanced.Report.Totals.OutputTokens)
	q.EffectiveEnd = q.RangeStart.Add(10*time.Hour + 5*time.Minute)
	q.EffectiveEnd = q.EffectiveEnd.Add(time.Nanosecond)
	afterBoundary := pgKeptBuild(t, store, f, q, true)
	assert.Equal(t, 400, afterBoundary.Report.Totals.OutputTokens)
	assert.Len(t, counter.transcriptReads, 1)
	assert.Len(t, counter.usageReads, 1)
}

func TestPGActivityReportInputRangeSwitchAfterPush(t *testing.T) {
	t.Parallel()
	local, syncer, store := newUsageJournalFixture(t)
	counter := &activityInputReadCounter{storeExecutor: store.pg}
	store.pg = counter
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	// Give each range enough real usage rows that all three exceed 128 MiB.
	_, err := store.DB().ExecContext(t.Context(), `INSERT INTO messages
		(session_id, ordinal, role, content, timestamp, model, token_usage, claude_message_id, claude_request_id,
		 usage_output_tokens, usage_web_search_requests)
		SELECT 'a', i + 1, 'assistant', '', '2026-06-14T10:01:00Z', 'kept-model',
		'{"output_tokens":1}', repeat('x', 500) || i::text, 'request', 1, 0
		FROM generate_series(1, 100000) AS i`)
	require.NoError(t, err)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	f := db.AnalyticsFilter{Timezone: "UTC"}
	var queries []activity.Query
	for _, input := range []activity.QueryInput{
		{Preset: "week", Date: "2026-06-14", Timezone: "UTC"},
		{Preset: "month", Date: "2026-06-14", Timezone: "UTC"},
		{Preset: "custom", From: "2026-01-01T00:00:00Z", To: "2027-01-01T00:00:00Z", Timezone: "UTC"},
	} {
		q, err := activity.ResolveQuery(input, now)
		require.NoError(t, err)
		queries = append(queries, q)
		pgKeptBuild(t, store, f, q, true)
	}
	pgKeptSession(t, local, "b", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	fresh, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	defer fresh.Close()
	for _, q := range queries {
		got := pgKeptBuild(t, store, f, q, true)
		requirePGKeptEqual(t, pgKeptBuild(t, fresh, f, q, true), got)
	}
	require.Len(t, counter.transcriptReads, 6)
	for _, ids := range counter.transcriptReads[3:] {
		assert.Equal(t, []string{"b"}, ids)
	}
	require.Len(t, counter.usageReads, 6)
	for _, ids := range counter.usageReads[3:] {
		assert.Equal(t, []string{"b"}, ids)
	}
}

func TestPGActivityReportInputRetry(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"budget", "cancel", "journal", "regressed", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			local, syncer, store := newUsageJournalFixture(t)
			pgKeptSession(t, local, "a", "project-a", "2026-06-14")
			pgKeptPush(t, syncer, false)
			counter := &activityInputReadCounter{storeExecutor: store.pg}
			store.pg = counter
			q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
			q.Partial, q.EffectiveEnd = true, q.RangeEnd.Add(-time.Hour)
			if mode == "budget" {
				store.runtimeState().activityInputs.MaxBytes = 1
			}
			if mode == "cancel" {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				counter.cancel = cancel
				_, err := store.BuildActivityReportArtifacts(ctx, f, q, nil)
				require.Error(t, err)
				counter.cancel = nil
			} else {
				pgKeptBuild(t, store, f, q, true)
			}
			if mode == "journal" {
				_, err := syncer.pg.ExecContext(t.Context(), dropUsageChangeJournalSQL)
				require.NoError(t, err)
				ensureUsageChangeJournalPG(t.Context(), syncer.pg)
			}
			if mode == "regressed" {
				_, err := syncer.pg.ExecContext(t.Context(), `UPDATE sync_metadata SET value=(value::bigint-100)::text WHERE key='usage_change_revision'`)
				require.NoError(t, err)
			}
			if mode == "unavailable" {
				_, err := syncer.pg.ExecContext(t.Context(), `UPDATE sync_metadata SET value='0' WHERE key='usage_change_journal_version'`)
				require.NoError(t, err)
			}
			before := len(counter.transcriptReads)
			got := pgKeptBuild(t, store, f, q, true)
			assert.Greater(t, len(counter.transcriptReads), before)
			assert.Equal(t, 500, got.Report.Totals.OutputTokens)
			assert.Len(t, got.Sessions, 1)
		})
	}
}

func TestPGActivityReportInputEmptySelectionAfterPush(t *testing.T) {
	t.Parallel()
	local, syncer, store := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC", Project: "project-a"}
	q.Partial, q.EffectiveEnd = true, q.RangeEnd.Add(-time.Hour)
	assert.Equal(t, 500, pgKeptBuild(t, store, f, q, true).Report.Totals.OutputTokens)
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "a", []db.Message{
		{SessionID: "a", Ordinal: 0, Role: "assistant", Content: "changed answer", Timestamp: "2026-06-14T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":700}`)},
	}))
	pgKeptPush(t, syncer, false)
	empty := pgKeptBuild(t, store, db.AnalyticsFilter{Timezone: "UTC", Project: "empty"}, q, true)
	assert.Empty(t, empty.Sessions)
	assert.Zero(t, empty.Report.Totals.OutputTokens)
	got := pgKeptBuild(t, store, f, q, true)
	assert.Equal(t, 700, got.Report.Totals.OutputTokens)
}

func TestPGActivityReportInputTranscriptAndCounts(t *testing.T) {
	t.Parallel()
	local, syncer, store := newUsageJournalFixture(t)
	messages := []db.Message{
		{SessionID: "a", Ordinal: 0, Role: "user", Content: "range start", Timestamp: "2026-06-14T00:00:00Z"},
		{SessionID: "a", Ordinal: 1, Role: "user", Content: "start", Timestamp: "2026-06-14T10:00:00.123456Z"},
		{SessionID: "a", Ordinal: 2, Role: "assistant", Content: "system", Timestamp: "2026-06-14T10:01:00Z", IsSystem: true},
		{SessionID: "a", Ordinal: 3, Role: "user", Content: "result", Timestamp: "2026-06-14T10:02:00Z", SourceSubtype: "tool_result"},
		{SessionID: "a", Ordinal: 4, Role: "assistant", Content: "boundary", Timestamp: "2026-06-14T10:05:00Z"},
	}
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{
		ID: "a", Project: "project-a", Machine: "test-machine", Agent: "claude",
		StartedAt: new("2026-06-14T00:00:00Z"), EndedAt: new("2026-06-14T10:05:00Z"), MessageCount: len(messages),
	}))
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "a", messages))
	pgKeptPush(t, syncer, false)
	require.NoError(t, store.activityTranscriptSource(t.Context(), []string{"a"}, func(id string, got []activity.TranscriptMessage) {
		assert.Equal(t, "a", id)
		require.Len(t, got, 5)
		assert.Equal(t, 1, got[1].Ordinal)
		assert.Equal(t, time.Date(2026, 6, 14, 10, 0, 0, 123456000, time.UTC), got[1].At)
		assert.Equal(t, "user", got[1].Role)
	}))
	counter := &activityInputReadCounter{storeExecutor: store.pg}
	store.pg = counter
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC", Project: "project-a"}
	q.Bucket = activity.BucketSpec{Unit: activity.BucketDay, NominalSeconds: 86400}
	q.Partial = true
	q.EffectiveEnd = q.RangeStart.Add(10*time.Hour + 5*time.Minute + 500*time.Nanosecond)
	before := pgKeptBuild(t, store, f, q, true)
	assert.Equal(t, 2, before.Report.Buckets[0].UserMessages)
	assert.Zero(t, before.Report.Buckets[0].AssistantMessages)
	q.EffectiveEnd = q.EffectiveEnd.Add(time.Microsecond)
	after := pgKeptBuild(t, store, f, q, true)
	assert.Equal(t, 2, after.Report.Buckets[0].UserMessages)
	assert.Equal(t, 1, after.Report.Buckets[0].AssistantMessages)
	q.RangeStart = q.RangeStart.Add(time.Microsecond)
	shifted := pgKeptBuild(t, store, f, q, true)
	assert.Equal(t, 1, shifted.Report.Buckets[0].UserMessages)
	assert.Equal(t, 1, shifted.Report.Buckets[0].AssistantMessages)
	assert.Len(t, counter.transcriptReads, 1)
	messages[2].IsSystem = false
	messages[3].SourceSubtype = ""
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "a", messages))
	pgKeptPush(t, syncer, false)
	changed := pgKeptBuild(t, store, f, q, true)
	assert.Equal(t, 2, changed.Report.Buckets[0].UserMessages)
	assert.Equal(t, 2, changed.Report.Buckets[0].AssistantMessages)
}

func TestPGActivityReportInputIdentity(t *testing.T) {
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	target := testPGURL(t)
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
	q.Partial, q.EffectiveEnd = true, q.RangeEnd.Add(-time.Hour)
	for _, mode := range []string{"retry", "denied", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			directory := t.TempDir()
			first := openPGActivityInputStore(t, target, syncer.schema, directory)
			query := q
			switch mode {
			case "retry":
				failOnce := &activityIdentityError{storeExecutor: first.pg}
				first.pg = failOnce
				pgKeptBuild(t, first, f, query, true)
				query.EffectiveEnd = query.EffectiveEnd.Add(time.Nanosecond)
				want := pgKeptBuild(t, first, f, query, true)
				require.NoError(t, first.Close())
				restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
				defer restarted.Close()
				reads := &activityInputReadCounter{storeExecutor: restarted.pg}
				restarted.pg = reads
				requirePGKeptEqual(t, want, pgKeptBuild(t, restarted, f, query, true))
				assert.Empty(t, reads.transcriptReads)
				assert.Empty(t, reads.usageReads)
				assert.GreaterOrEqual(t, failOnce.queries, 2)
			case "denied":
				defer first.Close()
				_, err := first.pg.ExecContext(t.Context(), `CREATE FUNCTION activity_identity_denied() RETURNS text LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'permission denied' USING ERRCODE = '42501'; END $$`)
				require.NoError(t, err)
				denied := &activityIdentityError{storeExecutor: first.pg, denied: true}
				first.pg = denied
				assert.Equal(t, 500, pgKeptBuild(t, first, f, query, true).Report.Totals.OutputTokens)
				for range 2 {
					query.EffectiveEnd = query.EffectiveEnd.Add(time.Nanosecond)
					assert.Equal(t, 500, pgKeptBuild(t, first, f, query, true).Report.Totals.OutputTokens)
				}
				assert.Equal(t, 1, denied.queries)
			case "mismatch":
				want := pgKeptBuild(t, first, f, query, true)
				slots := first.runtimeState().activityInputs.Snapshot()
				require.Len(t, slots, 1)
				slot, version := slots[0].Slot, pgActivityDiskVersion+slots[0].Version
				require.NoError(t, first.Close())
				data, ok := first.activityDisk.Get(slot, version)
				require.True(t, ok)
				rows, ok := kept.DecodeRows[kept.Part[pgActivityDiskPart]](data)
				require.True(t, ok)
				rows[0].Value.Identity = "different-system"
				data, err := kept.EncodeRows(rows, 256<<20, func(kept.Part[pgActivityDiskPart]) int64 { return 0 })
				require.NoError(t, err)
				first.activityDisk.Put(slot, version, data)
				restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
				defer restarted.Close()
				reads := &activityInputReadCounter{storeExecutor: restarted.pg}
				restarted.pg = reads
				requirePGKeptEqual(t, want, pgKeptBuild(t, restarted, f, query, true))
				require.NotEmpty(t, reads.transcriptReads)
				require.NotEmpty(t, reads.usageReads)
			}
		})
	}
}

func TestPGActivityReportInputRestartPeerAndSessions(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptSession(t, local, "peer", "project-b", "2026-06-14")
	pgKeptSession(t, local, "deleted", "project-a", "2026-06-14")
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "a", []db.Message{{SessionID: "a", Ordinal: 0, Role: "assistant", Content: "partial", Timestamp: "2026-06-14T10:00:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":5}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}))
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "peer", []db.Message{{SessionID: "peer", Ordinal: 0, Role: "assistant", Content: "complete", Timestamp: "2026-06-14T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":631}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}))
	pgKeptPush(t, syncer, false)
	target, directory := testPGURL(t), t.TempDir()
	q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC", Project: "project-a"}
	q.Partial, q.EffectiveEnd = true, q.RangeStart.Add(10*time.Hour+time.Minute)
	first := openPGActivityInputStore(t, target, syncer.schema, directory)
	before := pgKeptBuild(t, first, f, q, true)
	assert.Equal(t, 5, before.Report.Totals.OutputTokens)
	require.NoError(t, first.Close())
	q.EffectiveEnd = q.EffectiveEnd.Add(time.Nanosecond)
	require.NoError(t, local.DeleteSession(t.Context(), "deleted"))
	require.NoError(t, local.DeleteSession(t.Context(), "peer"))
	pgKeptSession(t, local, "added", "project-a", "2026-06-14")
	pgKeptSession(t, local, "new-peer", "project-b", "2026-06-14")
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "new-peer", []db.Message{{SessionID: "new-peer", Ordinal: 0, Role: "assistant", Content: "complete", Timestamp: "2026-06-14T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":700}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}))
	pgKeptPush(t, syncer, true)
	restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
	defer restarted.Close()
	counter := &activityInputReadCounter{storeExecutor: restarted.pg}
	restarted.pg = counter
	got := pgKeptBuild(t, restarted, f, q, true)
	assert.Equal(t, 1200, got.Report.Totals.OutputTokens)
	fresh, err := NewStore(t.Context(), target, syncer.schema, true)
	require.NoError(t, err)
	defer fresh.Close()
	requirePGKeptEqual(t, pgKeptBuild(t, fresh, f, q, true), got)
	require.Len(t, counter.transcriptReads, 1)
	assert.ElementsMatch(t, []string{"a", "added"}, counter.transcriptReads[0])
	require.Len(t, counter.usageReads, 1)
	assert.ElementsMatch(t, []string{"a", "added", "deleted", "new-peer", "peer"}, counter.usageReads[0])
}

func TestPGActivityReportInputRestartPrices(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	local, syncer, _ := newUsageJournalFixture(t)
	for _, id := range []string{"a", "b", "c"} {
		pgKeptSession(t, local, id, "project-a", "2026-06-14")
	}
	pgKeptSession(t, local, "peer", "project-b", "2026-06-14")
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "a", []db.Message{{SessionID: "a", Ordinal: 0, Role: "assistant", Content: "partial", Timestamp: "2026-06-14T10:00:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":5}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}))
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "peer", []db.Message{{SessionID: "peer", Ordinal: 0, Role: "assistant", Content: "complete", Timestamp: "2026-06-14T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":631}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}))
	pgKeptPush(t, syncer, false)
	target, directory := testPGURL(t), t.TempDir()
	q := pgDayQuery(t, "2026-06-14", "UTC")
	q.Partial, q.EffectiveEnd = true, q.RangeEnd.Add(-time.Hour)
	f := db.AnalyticsFilter{Timezone: "UTC", Project: "project-a"}
	first := openPGActivityInputStore(t, target, syncer.schema, directory)
	pgKeptBuild(t, first, db.AnalyticsFilter{Timezone: "UTC", Project: "project-b"}, q, true)
	want := pgKeptBuild(t, first, f, q, true)
	slots := first.runtimeState().activityInputs.Snapshot()
	require.Len(t, slots, 1)
	saved := slots[0].Rows[0]
	require.NotNil(t, saved.priced)
	require.NoError(t, first.Close())
	restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
	defer restarted.Close()
	loaded, ok := restarted.loadActivityInputs(slots[0].Slot, slots[0].Version, saved.identity)
	require.True(t, ok)
	require.Equal(t, saved.usage, loaded.usage)
	require.NotNil(t, loaded.priced)
	require.Equal(t, saved.priced.rows, loaded.priced.rows)
	reads := &activityInputReadCounter{storeExecutor: restarted.pg}
	restarted.pg = reads
	got := pgKeptBuild(t, restarted, f, q, true)
	requirePGKeptEqual(t, want, got)
	wantPricing, err := json.Marshal(want.Report.Pricing)
	require.NoError(t, err)
	gotPricing, err := json.Marshal(got.Report.Pricing)
	require.NoError(t, err)
	assert.Equal(t, string(wantPricing), string(gotPricing))
	assert.Empty(t, reads.transcriptReads)
	assert.Empty(t, reads.usageReads)
	other := db.AnalyticsFilter{Timezone: "UTC", Project: "project-b"}
	fresh, err := NewStore(t.Context(), target, syncer.schema, true)
	require.NoError(t, err)
	defer fresh.Close()
	requirePGKeptEqual(t, pgKeptBuild(t, fresh, other, q, true), pgKeptBuild(t, restarted, other, q, true))
}

func TestPGActivityReportInputRestartPriceInvalidation(t *testing.T) {
	for _, mode := range []string{"pricing", "push", "clock", "bad index", "duplicate index"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CACHE_DIRECTORY", t.TempDir())
			local, syncer, _ := newUsageJournalFixture(t)
			for _, id := range []string{"a", "b"} {
				pgKeptSession(t, local, id, "project-a", "2026-06-14")
			}
			pgKeptSession(t, local, "future", "project-a", "2026-06-14")
			require.NoError(t, local.ReplaceSessionMessages(t.Context(), "future", []db.Message{{SessionID: "future", Ordinal: 0, Role: "assistant", Content: "later", Timestamp: "2026-06-14T10:05:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":300}`)}}))
			pgKeptPush(t, syncer, false)
			target, directory := testPGURL(t), t.TempDir()
			q, f := pgDayQuery(t, "2026-06-14", "UTC"), db.AnalyticsFilter{Timezone: "UTC"}
			q.Partial, q.EffectiveEnd = true, q.RangeStart.Add(10*time.Hour+2*time.Minute)
			first := openPGActivityInputStore(t, target, syncer.schema, directory)
			if mode == "push" {
				first.SetCustomPricing(map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}})
			}
			before := pgKeptBuild(t, first, f, q, true)
			slots := first.runtimeState().activityInputs.Snapshot()
			require.Len(t, slots, 1)
			slot, version := slots[0].Slot, pgActivityDiskVersion+slots[0].Version
			require.NoError(t, first.Close())
			if mode == "bad index" || mode == "duplicate index" {
				data, ok := first.activityDisk.Get(slot, version)
				require.True(t, ok)
				rows, ok := kept.DecodeRows[kept.Part[pgActivityDiskPart]](data)
				require.True(t, ok)
				require.NotNil(t, rows[0].Value.Priced)
				require.GreaterOrEqual(t, len(rows[0].Value.Priced.Rows), 2)
				if mode == "bad index" {
					rows[0].Value.Priced.Rows[0].Index = int32(len(rows[0].Value.Usage))
				} else {
					rows[0].Value.Priced.Rows[1].Index = rows[0].Value.Priced.Rows[0].Index
				}
				data, err := kept.EncodeRows(rows, 256<<20, func(kept.Part[pgActivityDiskPart]) int64 { return 0 })
				require.NoError(t, err)
				first.activityDisk.Put(slot, version, data)
			}
			if mode == "push" {
				pgKeptSession(t, local, "c", "project-a", "2026-06-14")
				require.NoError(t, local.ReplaceSessionMessages(t.Context(), "c", []db.Message{{SessionID: "c", Ordinal: 0, Role: "assistant", Content: "new usage", Timestamp: "2026-06-14T10:01:00Z", Model: "kept-model", TokenUsage: []byte(`{"output_tokens":700}`)}}))
				pgKeptPush(t, syncer, false)
			}
			if mode == "clock" {
				require.False(t, slots[0].Rows[0].priced.next.IsZero())
				q.EffectiveEnd = q.RangeEnd.Add(-time.Hour)
			}
			restarted := openPGActivityInputStore(t, target, syncer.schema, directory)
			defer restarted.Close()
			fresh, err := NewStore(t.Context(), target, syncer.schema, true)
			require.NoError(t, err)
			defer fresh.Close()
			if mode == "pricing" || mode == "push" {
				rates := map[string]config.CustomModelRate{"kept-model": {OutputMicrodollarsPerMTok: 20_000_000}}
				restarted.SetCustomPricing(rates)
				fresh.SetCustomPricing(rates)
			}
			if mode == "bad index" || mode == "duplicate index" {
				loaded, ok := restarted.loadActivityInputs(slots[0].Slot, slots[0].Version, slots[0].Rows[0].identity)
				require.True(t, ok)
				require.Nil(t, loaded.priced)
			}
			got := pgKeptBuild(t, restarted, f, q, true)
			requirePGKeptEqual(t, pgKeptBuild(t, fresh, f, q, true), got)
			if mode == "push" {
				assert.Equal(t, 1000, before.Report.Totals.OutputTokens)
				assert.Equal(t, 1700, got.Report.Totals.OutputTokens)
				assert.Equal(t, money.MustParseDollars("0.034"), got.Report.Totals.Cost)
				assert.NotEqual(t, before.Report.Totals.Cost, got.Report.Totals.Cost)
			}
		})
	}
}
