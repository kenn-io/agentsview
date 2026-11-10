//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

type activityInputReadCounter struct {
	storeExecutor
	mu                          sync.Mutex
	transcriptReads, usageReads [][]string
	cancel                      context.CancelFunc
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

func TestPGActivityReportInputClock(t *testing.T) {
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
	q.EffectiveEnd = q.EffectiveEnd.Add(5 * time.Minute)
	after := pgKeptBuild(t, store, f, q, true)
	assert.Equal(t, 400, after.Report.Totals.OutputTokens)
	assert.Len(t, counter.transcriptReads, 1)
	assert.Len(t, counter.usageReads, 1)
	assert.NotEqual(t, before.Report.Totals, after.Report.Totals)
	fresh, err := NewStore(t.Context(), testPGURL(t), syncer.schema, true)
	require.NoError(t, err)
	defer fresh.Close()
	requirePGKeptEqual(t, pgKeptBuild(t, fresh, f, q, true), after)
}

func TestPGActivityReportInputRangeSwitchAfterPush(t *testing.T) {
	local, syncer, store := newUsageJournalFixture(t)
	counter := &activityInputReadCounter{storeExecutor: store.pg}
	store.pg = counter
	pgKeptSession(t, local, "a", "project-a", "2026-06-14")
	pgKeptPush(t, syncer, false)
	// Give each range enough real usage rows that all three exceed 128 MiB.
	_, err := store.DB().ExecContext(t.Context(), `INSERT INTO messages
		(session_id, ordinal, role, content, timestamp, model, token_usage, claude_message_id, claude_request_id)
		SELECT 'a', i + 1, 'assistant', '', '2026-06-14T10:01:00Z', 'kept-model',
		'{"output_tokens":1}', repeat('x', 500) || i::text, 'request'
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
