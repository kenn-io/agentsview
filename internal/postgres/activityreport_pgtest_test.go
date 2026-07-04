//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// pgDayQuery resolves a single-day "day" Query for date/tz against a fixed
// far-future now, so the candidate range is the full local day and the
// report is never partial regardless of the wall clock.
func pgDayQuery(t *testing.T, date, tz string) activity.Query {
	t.Helper()
	now, err := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	require.NoError(t, err)
	q, err := activity.ResolveQuery(
		activity.QueryInput{Preset: "day", Date: date, Timezone: tz}, now)
	require.NoError(t, err)
	return q
}

// seedPGDailyFixture inserts two overlapping sessions on 2026-06-16
// (UTC), each with two timestamped messages, mirroring the SQLite
// fixture in internal/db/activityreport_test.go.
func seedPGDailyFixture(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES
			('a', 'test-machine', 'proj1', 'claude',
			 '2026-06-16T10:00:00Z'::timestamptz,
			 '2026-06-16T10:02:00Z'::timestamptz, 2, 1),
			('b', 'test-machine', 'proj2', 'codex',
			 '2026-06-16T10:01:00Z'::timestamptz,
			 '2026-06-16T10:03:00Z'::timestamptz, 2, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model
		) VALUES
			('a', 1, 'user', 'x',
			 '2026-06-16T10:00:00Z'::timestamptz, 1, ''),
			('a', 2, 'assistant', 'x',
			 '2026-06-16T10:02:00Z'::timestamptz, 1, 'opus'),
			('b', 1, 'user', 'x',
			 '2026-06-16T10:01:00Z'::timestamptz, 1, ''),
			('b', 2, 'assistant', 'x',
			 '2026-06-16T10:03:00Z'::timestamptz, 1, 'gpt5')`)
	require.NoError(t, err, "insert messages")
}

func TestPGGetActivityReport(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_daily_report_test")
	ctx := context.Background()
	seedPGDailyFixture(t, store)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		pgDayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 2, r.Peak.Agents)
	assert.Equal(t, 2, r.Totals.Sessions)
	assert.GreaterOrEqual(t, len(r.ByModel), 2)
}

// TestPGGetActivityReportOpenSessionWithInRangeMessageIncluded confirms a
// still-open session (no ended_at) that started before the range but has a
// message inside it is not dropped. The effective-end fallback uses the
// session's latest message timestamp, not started_at, matching SQLite and
// DuckDB. Mirrors the SQLite
// TestGetActivityReport_OpenSessionWithInRangeMessageIncluded.
func TestPGGetActivityReportOpenSessionWithInRangeMessageIncluded(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_daily_report_open_test")
	ctx := context.Background()

	// Started the day before, never closed (ended_at NULL), active in-range.
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'open', 'test-machine', 'proj1', 'claude',
			'2026-06-15T23:00:00Z'::timestamptz, NULL, 2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model
		) VALUES
			('open', 1, 'user', 'x',
			 '2026-06-16T10:00:00Z'::timestamptz, 1, ''),
			('open', 2, 'assistant', 'x',
			 '2026-06-16T10:02:00Z'::timestamptz, 1, 'opus')`)
	require.NoError(t, err, "insert messages")

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		pgDayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := make(map[string]struct{}, len(r.BySession))
	for _, s := range r.BySession {
		ids[s.SessionID] = struct{}{}
	}
	assert.Contains(t, ids, "open",
		"open session active in-range must not be dropped by the started_at fallback")
	assert.Equal(t, 1, r.Totals.Sessions)
}

// TestPGGetActivityReportUsageCostAndTokens exercises the PG usage
// union + cost path: a single priced assistant message must surface
// its output tokens and computed cost in the day totals, matching
// the SQLite reference behavior.
func TestPGGetActivityReportUsageCostAndTokens(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_daily_report_usage_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('claude-sonnet-4-20250514', 3, 15, 0, 0, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			's1', 'test-machine', 'proj1', 'claude',
			'2026-06-16T10:30:00Z'::timestamptz,
			'2026-06-16T10:30:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage
		) VALUES (
			's1', 0, 'assistant', 'x',
			'2026-06-16T10:30:00Z'::timestamptz, 1,
			'claude-sonnet-4-20250514',
			'{"input_tokens":1000,"output_tokens":500}'
		)`)
	require.NoError(t, err, "insert message")

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		pgDayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 1, r.Totals.Sessions)
	assert.Equal(t, 500, r.Totals.OutputTokens)
	// Cost = (1000*3 + 500*15) / 1e6 = 0.0105
	assert.InDelta(t, 0.0105, r.Totals.Cost, 1e-9)
}

func TestPGGetActivityReportPricingModelsOnlyIncludeDedupSurvivors(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_daily_report_pricing_survivor_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES
			('kept-model', 3, 15, 0, 0, 'seed'),
			('discarded-model', 3, 15, 0, 0, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES
			('earlier', 'test-machine', 'proj1', 'claude',
			 '2026-06-16T10:30:00Z'::timestamptz,
			 '2026-06-16T10:30:00Z'::timestamptz, 1, 1),
			('later', 'test-machine', 'proj1', 'claude',
			 '2026-06-16T10:31:00Z'::timestamptz,
			 '2026-06-16T10:31:00Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage,
			claude_message_id, claude_request_id
		) VALUES
			('earlier', 0, 'assistant', 'x',
			 '2026-06-16T10:30:00Z'::timestamptz, 1,
			 'kept-model',
			 '{"input_tokens":1000,"output_tokens":500}', 'M1', 'R1'),
			('later', 0, 'assistant', 'x',
			 '2026-06-16T10:31:00Z'::timestamptz, 1,
			 'discarded-model',
			 '{"input_tokens":2000,"output_tokens":900}', 'M1', 'R1')`)
	require.NoError(t, err, "insert messages")

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		pgDayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens)
	require.NotNil(t, r.Pricing)
	assert.Contains(t, r.Pricing.Models, "kept-model")
	assert.NotContains(t, r.Pricing.Models, "discarded-model")
}

// TestPGGetActivityReportExcludesOtherDays confirms the candidate-session
// window plus the usage ts-bounds keep a session whose only activity
// falls outside the target day from contributing to that day.
func TestPGGetActivityReportExcludesOtherDays(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_daily_report_otherday_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES
			('today', 'test-machine', 'proj1', 'claude',
			 '2026-06-16T10:00:00Z'::timestamptz,
			 '2026-06-16T10:02:00Z'::timestamptz, 2, 1),
			('yesterday', 'test-machine', 'proj2', 'codex',
			 '2026-06-10T10:00:00Z'::timestamptz,
			 '2026-06-10T10:02:00Z'::timestamptz, 2, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model
		) VALUES
			('today', 1, 'user', 'x',
			 '2026-06-16T10:00:00Z'::timestamptz, 1, ''),
			('today', 2, 'assistant', 'x',
			 '2026-06-16T10:02:00Z'::timestamptz, 1, 'opus'),
			('yesterday', 1, 'user', 'x',
			 '2026-06-10T10:00:00Z'::timestamptz, 1, ''),
			('yesterday', 2, 'assistant', 'x',
			 '2026-06-10T10:02:00Z'::timestamptz, 1, 'gpt5')`)
	require.NoError(t, err, "insert messages")

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		pgDayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	// Only the in-day session has timed intervals on 2026-06-16.
	assert.Equal(t, 1, r.Peak.Agents)
	require.Len(t, r.ByAgent, 1)
	assert.Equal(t, "claude", r.ByAgent[0].Key)
}

// reportSessionIDsPG collects the session IDs present in a report's
// BySession rows.
func reportSessionIDsPG(sessions []activity.SessionRow) map[string]struct{} {
	out := make(map[string]struct{}, len(sessions))
	for _, s := range sessions {
		out[s.SessionID] = struct{}{}
	}
	return out
}

// TestPGGetActivityReportPriorDayWithinPadExcluded confirms the PG
// candidate window uses the EXACT local day, not the +/-14h padded
// bounds: a session that began and ended on the prior day but lands
// inside the pad must NOT appear in the target day's report.
func TestPGGetActivityReportPriorDayWithinPadExcluded(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_daily_report_pad_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES
			('today', 'test-machine', 'proj1', 'claude',
			 '2026-06-16T10:00:00Z'::timestamptz,
			 '2026-06-16T10:02:00Z'::timestamptz, 2, 1),
			('prior', 'test-machine', 'proj2', 'codex',
			 '2026-06-15T12:00:00Z'::timestamptz,
			 '2026-06-15T12:05:00Z'::timestamptz, 2, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model
		) VALUES
			('today', 1, 'user', 'x',
			 '2026-06-16T10:00:00Z'::timestamptz, 1, ''),
			('today', 2, 'assistant', 'x',
			 '2026-06-16T10:02:00Z'::timestamptz, 1, 'opus')`)
	require.NoError(t, err, "insert messages")

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		pgDayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDsPG(r.BySession)
	assert.Contains(t, ids, "today")
	assert.NotContains(t, ids, "prior", "prior-day session must not leak in")
	assert.Equal(t, 1, r.Totals.Sessions)
	assert.Equal(t, 0, r.Totals.UntimedSessions)
}

// TestPGGetActivityReportExcludesIneligibleUsage confirms the PG usage
// union applies the same eligibility filters as GetDailyUsage: a
// synthetic-model message carrying real token_usage must not inflate
// the day totals.
func TestPGGetActivityReportExcludesIneligibleUsage(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_daily_report_eligible_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('claude-sonnet-4-20250514', 3, 15, 0, 0, 'seed')`)
	require.NoError(t, err, "insert pricing")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			's1', 'test-machine', 'proj1', 'claude',
			'2026-06-16T10:30:00Z'::timestamptz,
			'2026-06-16T10:31:00Z'::timestamptz, 2, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage
		) VALUES
			('s1', 0, 'assistant', 'x',
			 '2026-06-16T10:30:00Z'::timestamptz, 1,
			 'claude-sonnet-4-20250514',
			 '{"input_tokens":1000,"output_tokens":500}'),
			('s1', 1, 'assistant', 'y',
			 '2026-06-16T10:31:00Z'::timestamptz, 1,
			 '<synthetic>',
			 '{"input_tokens":9000,"output_tokens":7000}')`)
	require.NoError(t, err, "insert messages")

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		pgDayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens, "synthetic message excluded")
	assert.InDelta(t, 0.0105, r.Totals.Cost, 1e-9)
}

// TestPGGetActivityReportDedupsAcrossChunks confirms the PG usage fetch's
// global re-sort across maxPGVars-sized ID chunks (activityReportUsage in
// internal/postgres/activityreport.go) orders rows by timestamp across the
// whole candidate set, not per chunk. The aggregator's first-seen-wins
// dedup relies on that order: the same (claude_message_id,
// claude_request_id) can recur in two sessions (resumed/forked) that fall
// in DIFFERENT chunks, and the globally-earliest row must survive.
//
// The candidate IDs are passed to activityReportUsage explicitly, so the
// chunk split is deterministic and never depends on PostgreSQL's scan
// order. The slice is [dup-b, 500 fillers, dup-a]: dup-b (the LATER
// timestamp) lands in the first chunk (indices 0-499) and dup-a (the
// EARLIER timestamp) in the second (indices 500-501). Only a global
// re-sort by timestamp reorders the fetched rows to [dup-a, dup-b]; a
// regression that appended per-chunk results in chunk order would yield
// [dup-b, dup-a] and fail the ordering assertion below. The fillers are
// placeholder IDs with no rows in the DB -- they exist only to push dup-a
// past the 500-variable chunk boundary.
func TestPGGetActivityReportDedupsAcrossChunks(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_daily_report_chunk_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('claude-sonnet-4-20250514', 3, 15, 0, 0, 'seed')`)
	require.NoError(t, err, "insert pricing")

	// dup-a: earlier timestamp and 500 output tokens -> the correct global
	// survivor of the shared (claude_message_id, claude_request_id) key.
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'dup-a', 'test-machine', 'proj1', 'claude',
			'2026-06-16T10:00:00Z'::timestamptz,
			'2026-06-16T10:00:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err, "insert dup-a session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage,
			claude_message_id, claude_request_id
		) VALUES (
			'dup-a', 0, 'assistant', 'x',
			'2026-06-16T10:00:00Z'::timestamptz, 1,
			'claude-sonnet-4-20250514',
			'{"input_tokens":250,"output_tokens":500}', 'M1', 'R1'
		)`)
	require.NoError(t, err, "insert dup-a message")

	// dup-b: same dedup identity as dup-a (claude_message_id,
	// claude_request_id) but a later timestamp and 900 output tokens; the
	// first-seen dedup must drop it in favor of dup-a.
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'dup-b', 'test-machine', 'proj1', 'claude',
			'2026-06-16T10:05:00Z'::timestamptz,
			'2026-06-16T10:05:00Z'::timestamptz, 1, 1
		)`)
	require.NoError(t, err, "insert dup-b session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage,
			claude_message_id, claude_request_id
		) VALUES (
			'dup-b', 0, 'assistant', 'x',
			'2026-06-16T10:05:00Z'::timestamptz, 1,
			'claude-sonnet-4-20250514',
			'{"input_tokens":450,"output_tokens":900}', 'M1', 'R1'
		)`)
	require.NoError(t, err, "insert dup-b message")

	// Build the candidate ID slice explicitly so the chunk split is
	// deterministic: dup-b in chunk 1 (later ts), dup-a in chunk 2 (earlier
	// ts), forcing the shared dedup identity across the maxPGVars boundary.
	ids := make([]string, 0, maxPGVars+2)
	ids = append(ids, "dup-b")
	for i := 0; i < maxPGVars; i++ {
		ids = append(ids, fmt.Sprintf("fill-%d", i))
	}
	ids = append(ids, "dup-a")
	require.Greater(t, len(ids), maxPGVars,
		"candidate IDs must exceed one chunk to span the boundary")

	q := pgDayQuery(t, "2026-06-16", "UTC")
	lower := paddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upper := paddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)
	usage, _, err := store.activityReportUsage(ctx, ids, lower, upper, q)
	require.NoError(t, err)

	// Only dup-a and dup-b carry eligible usage; the fillers have no rows, and
	// the shared dedup identity leaves one survivor.
	var shared []activity.UsageRow
	for _, u := range usage {
		if u.SessionID == "dup-a" || u.SessionID == "dup-b" {
			shared = append(shared, u)
		}
	}
	require.Len(t, shared, 1,
		"only one dedup-identity row survives across the chunk boundary")
	require.NotEmpty(t, shared[0].ClaudeMessageID, "rows carry a message id")
	// The global re-sort by timestamp must place dup-a (10:00, fetched in
	// the LATER chunk) before dup-b (10:05, fetched in the EARLIER chunk).
	// A per-chunk-order regression keeps dup-b and fails here.
	assert.Equal(t, "dup-a", shared[0].SessionID,
		"earlier-timestamp row sorts first despite its later chunk")
	assert.Equal(t, 500, shared[0].OutputTokens, "dup-a survives first-seen")
}

func TestPGGetActivityReportUsageDedupFallsBackToSourceUUID(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_daily_report_sourceuuid_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO model_pricing (
			model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		) VALUES ('claude-sonnet-4-20250514', 3, 15, 0, 0, 'seed')`)
	require.NoError(t, err, "insert pricing")

	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES
			('earlier', 'test-machine', 'proj1', 'claude',
			 '2026-06-16T10:30:00Z'::timestamptz,
			 '2026-06-16T10:30:00Z'::timestamptz, 1, 1),
			('later', 'test-machine', 'proj2', 'claude',
			 '2026-06-16T10:30:01Z'::timestamptz,
			 '2026-06-16T10:30:01Z'::timestamptz, 1, 1)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage,
			claude_message_id, source_uuid
		) VALUES
			('earlier', 0, 'assistant', 'x',
			 '2026-06-16T10:30:00Z'::timestamptz, 1,
			 'claude-sonnet-4-20250514',
			 '{"input_tokens":1000,"output_tokens":500}', 'M1', 'SRC1'),
			('later', 0, 'assistant', 'x',
			 '2026-06-16T10:30:01Z'::timestamptz, 1,
			 'claude-sonnet-4-20250514',
			 '{"input_tokens":1000,"output_tokens":900}', 'M1', 'SRC1')`)
	require.NoError(t, err, "insert messages")

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		pgDayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens,
		"incomplete Claude pairs fall back to source_uuid dedup in activity reports")
}

// TestPGGetActivityReportAutomationFilterAndSessionSplit confirms the shared
// AnalyticsFilter automation class is honored through the PG analytics WHERE
// builder and that the Totals carry the automated/interactive session-count
// split. Mirrors the SQLite
// TestGetActivityReport_AutomationFilterAndSessionSplit.
func TestPGGetActivityReportAutomationFilterAndSessionSplit(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_daily_report_automation_test")
	ctx := context.Background()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count, is_automated
		) VALUES
			('auto1', 'test-machine', 'proj1', 'claude',
			 '2026-06-16T10:00:00Z'::timestamptz,
			 '2026-06-16T10:02:00Z'::timestamptz, 2, 1, TRUE),
			('auto2', 'test-machine', 'proj1', 'claude',
			 '2026-06-16T11:00:00Z'::timestamptz,
			 '2026-06-16T11:02:00Z'::timestamptz, 2, 1, TRUE),
			('human', 'test-machine', 'proj2', 'codex',
			 '2026-06-16T12:00:00Z'::timestamptz,
			 '2026-06-16T12:02:00Z'::timestamptz, 2, 1, FALSE)`)
	require.NoError(t, err, "insert sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model
		) VALUES
			('auto1', 1, 'user', 'x', '2026-06-16T10:00:00Z'::timestamptz, 1, ''),
			('auto1', 2, 'assistant', 'x', '2026-06-16T10:02:00Z'::timestamptz, 1, 'opus'),
			('auto2', 1, 'user', 'x', '2026-06-16T11:00:00Z'::timestamptz, 1, ''),
			('auto2', 2, 'assistant', 'x', '2026-06-16T11:02:00Z'::timestamptz, 1, 'opus'),
			('human', 1, 'user', 'x', '2026-06-16T12:00:00Z'::timestamptz, 1, ''),
			('human', 2, 'assistant', 'x', '2026-06-16T12:02:00Z'::timestamptz, 1, 'gpt5')`)
	require.NoError(t, err, "insert messages")

	tests := []struct {
		name            string
		filter          db.AnalyticsFilter
		wantAutomated   int
		wantInteractive int
		wantIDs         []string
	}{
		{
			name:            "all keeps both classes",
			filter:          db.AnalyticsFilter{Timezone: "UTC"},
			wantAutomated:   2,
			wantInteractive: 1,
			wantIDs:         []string{"auto1", "auto2", "human"},
		},
		{
			name:            "exclude automated keeps interactive only",
			filter:          db.AnalyticsFilter{Timezone: "UTC", ExcludeAutomated: true},
			wantAutomated:   0,
			wantInteractive: 1,
			wantIDs:         []string{"human"},
		},
		{
			name:            "exclude interactive keeps automated only",
			filter:          db.AnalyticsFilter{Timezone: "UTC", ExcludeInteractive: true},
			wantAutomated:   2,
			wantInteractive: 0,
			wantIDs:         []string{"auto1", "auto2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := store.GetActivityReport(ctx, tc.filter,
				pgDayQuery(t, "2026-06-16", "UTC"))
			require.NoError(t, err)
			assert.Equal(t, len(tc.wantIDs), r.Totals.Sessions)
			assert.Equal(t, tc.wantAutomated, r.Totals.AutomatedSessions)
			assert.Equal(t, tc.wantInteractive, r.Totals.InteractiveSessions)
			ids := reportSessionIDsPG(r.BySession)
			require.Len(t, ids, len(tc.wantIDs))
			for _, id := range tc.wantIDs {
				assert.Contains(t, ids, id)
			}
		})
	}
}
