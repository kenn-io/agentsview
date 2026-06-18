package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

func reportSessionIDs(sessions []activity.SessionRow) map[string]struct{} {
	out := make(map[string]struct{}, len(sessions))
	for _, s := range sessions {
		out[s.SessionID] = struct{}{}
	}
	return out
}

// dayQuery resolves a single-day "day" Query for date/tz against a fixed
// far-future now, so the candidate range is the full local day and the
// report is never partial regardless of the wall clock.
func dayQuery(t *testing.T, date, tz string) activity.Query {
	t.Helper()
	now, err := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	require.NoError(t, err)
	q, err := activity.ResolveQuery(
		activity.QueryInput{Preset: "day", Date: date, Timezone: tz}, now)
	require.NoError(t, err)
	return q
}

func seedMessage(
	t *testing.T, d *DB, sid string, ordinal int, role, ts, model string,
) {
	t.Helper()
	insertMessages(t, d, Message{
		SessionID: sid,
		Ordinal:   ordinal,
		Role:      role,
		Content:   "x",
		Timestamp: ts,
		Model:     model,
	})
}

func TestGetActivityReport_BasicConcurrency(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	// Two overlapping sessions on 2026-06-16 (UTC), each two messages.
	// started_at/ended_at are set explicitly so the candidate-session
	// window anchors on the target day regardless of the wall clock; a
	// created_at fallback would drift to the prior/next day when the
	// suite runs near UTC midnight.
	insertSession(t, d, "a", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:02:00Z")
	})
	seedMessage(t, d, "a", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "a", 2, "assistant", "2026-06-16T10:02:00Z", "opus")
	insertSession(t, d, "b", "proj2", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2026-06-16T10:01:00Z")
		s.EndedAt = Ptr("2026-06-16T10:03:00Z")
	})
	seedMessage(t, d, "b", 1, "user", "2026-06-16T10:01:00Z", "")
	seedMessage(t, d, "b", 2, "assistant", "2026-06-16T10:03:00Z", "gpt5")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 2, r.Peak.Agents)
	assert.Equal(t, 2, r.Totals.Sessions)
	assert.GreaterOrEqual(t, len(r.ByModel), 2)
}

func TestGetActivityReport_UsageCostAndTokens(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "s1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:30:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "s1",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "x",
		Timestamp:  "2026-06-16T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 1, r.Totals.Sessions)
	assert.Equal(t, 500, r.Totals.OutputTokens)
	// Cost = (1000*3 + 500*15) / 1e6 = 0.0105
	assert.InDelta(t, 0.0105, r.Totals.Cost, 1e-9)
}

// TestGetActivityReport_ExcludesOtherDays confirms the candidate-session
// window and the usage ts-bounds keep a session whose only activity
// falls outside the target day from contributing to that day.
func TestGetActivityReport_ExcludesOtherDays(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "today", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:02:00Z")
	})
	seedMessage(t, d, "today", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "today", 2, "assistant", "2026-06-16T10:02:00Z", "opus")

	insertSession(t, d, "yesterday", "proj2", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2026-06-10T10:00:00Z")
		s.EndedAt = Ptr("2026-06-10T10:02:00Z")
	})
	seedMessage(t, d, "yesterday", 1, "user", "2026-06-10T10:00:00Z", "")
	seedMessage(t, d, "yesterday", 2, "assistant", "2026-06-10T10:02:00Z", "gpt5")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	// Only the in-day session has timed intervals on 2026-06-16.
	assert.Equal(t, 1, r.Peak.Agents)
	require.Len(t, r.ByAgent, 1)
	assert.Equal(t, "claude", r.ByAgent[0].Key)
}

// TestGetActivityReport_PriorDayWithinPadExcluded confirms the candidate
// window uses the EXACT local day, not the +/-14h padded bounds: a
// session that began and ended on the prior day but lands inside the
// pad must NOT appear as an untimed session in the target day's report.
func TestGetActivityReport_PriorDayWithinPadExcluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "today", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:02:00Z")
	})
	seedMessage(t, d, "today", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "today", 2, "assistant", "2026-06-16T10:02:00Z", "opus")

	// Prior-day session at 2026-06-15T12:00Z: within the old -14h pad
	// (2026-06-15T10:00Z) but outside the exact 2026-06-16 UTC window.
	insertSession(t, d, "prior", "proj2", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2026-06-15T12:00:00Z")
		s.EndedAt = Ptr("2026-06-15T12:05:00Z")
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "today")
	assert.NotContains(t, ids, "prior", "prior-day session must not leak in")
	assert.Equal(t, 1, r.Totals.Sessions)
	assert.Equal(t, 0, r.Totals.UntimedSessions)
}

// TestGetActivityReport_UntimedSessionOnDayIncluded confirms a session
// that started on the target day but has no timestamped messages still
// appears in the report as an untimed candidate.
func TestGetActivityReport_UntimedSessionOnDayIncluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "untimed", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T09:00:00Z")
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "untimed")
	assert.Equal(t, 1, r.Totals.UntimedSessions)
}

// TestGetActivityReport_EmptyStringEndedAtIncluded confirms the overlap
// predicate uses NULLIF so a session with an empty-string ended_at but a
// valid started_at on the target day is not excluded by COALESCE
// treating an empty string as a real upper bound.
func TestGetActivityReport_EmptyStringEndedAtIncluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "empty-end", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T09:00:00Z")
		s.EndedAt = Ptr("")
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "empty-end", "empty ended_at must fall back to started_at")
}

// TestGetActivityReport_SubSecondDayStartIncluded confirms a session whose
// only activity lands in the first sub-second of the day is not dropped by
// SQLite's lexicographic TEXT comparison. A stored RFC3339Nano value like
// "2026-06-14T00:00:00.123Z" sorts before a Z-suffixed day-start bound
// ("2026-06-14T00:00:00Z") because '.' < 'Z', so a Z-suffixed bound would
// wrongly exclude it. The zone-less day bound fixes that.
func TestGetActivityReport_SubSecondDayStartIncluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "subsec", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-14T00:00:00.123Z")
		s.EndedAt = Ptr("2026-06-14T00:00:00.123Z")
	})
	seedMessage(t, d, "subsec", 0, "user", "2026-06-14T00:00:00.123Z", "")
	seedMessage(t, d, "subsec", 1, "assistant", "2026-06-14T00:00:00.456Z", "opus")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "subsec",
		"first-sub-second session must not be dropped by the day-start bound")
	assert.GreaterOrEqual(t, r.Totals.Sessions, 1)
}

// TestGetActivityReport_ExcludesIneligibleUsage confirms the usage union
// applies the same eligibility filters as GetDailyUsage: a message with
// an empty model and empty token_usage must not inflate the day totals.
func TestGetActivityReport_ExcludesIneligibleUsage(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "s1", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:31:00Z")
	})
	insertMessages(t, d, Message{
		SessionID:  "s1",
		Ordinal:    0,
		Role:       "assistant",
		Content:    "x",
		Timestamp:  "2026-06-16T10:30:00Z",
		Model:      "claude-sonnet-4-20250514",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})
	// Ineligible: a synthetic-model message carrying real token_usage.
	// usageMessageEligibility drops m.model == '<synthetic>', so these
	// tokens must NOT leak into the day totals even though the blob is
	// non-empty.
	insertMessages(t, d, Message{
		SessionID:  "s1",
		Ordinal:    1,
		Role:       "assistant",
		Content:    "y",
		Timestamp:  "2026-06-16T10:31:00Z",
		Model:      "<synthetic>",
		TokenUsage: json.RawMessage(`{"input_tokens":9000,"output_tokens":7000}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens, "synthetic message excluded")
	assert.InDelta(t, 0.0105, r.Totals.Cost, 1e-9)
}

// TestGetActivityReport_HourlyRange exercises a multi-day custom range so
// the bucket auto-policy selects hourly buckets, and confirms the fetch
// window spans the whole range: a session whose only activity falls on the
// middle day populates the hourly bucket that contains it.
func TestGetActivityReport_HourlyRange(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "mid", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-17T10:00:00Z")
		s.EndedAt = Ptr("2026-06-17T10:30:00Z")
	})
	seedMessage(t, d, "mid", 1, "user", "2026-06-17T10:00:00Z", "")
	seedMessage(t, d, "mid", 2, "assistant", "2026-06-17T10:30:00Z", "opus")

	// 3-day span -> hourly buckets per the auto policy.
	now, err := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	require.NoError(t, err)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "custom", Timezone: "UTC",
		From: "2026-06-16T00:00:00Z", To: "2026-06-19T00:00:00Z",
	}, now)
	require.NoError(t, err)

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"}, q)
	require.NoError(t, err)
	assert.Equal(t, "hour", r.BucketUnit)
	assert.Equal(t, 72, r.BucketCount, "3 days of hourly buckets")
	// The 30-min gap caps to 5 min and lands in the 2026-06-17T10:00 bucket.
	var found bool
	for _, b := range r.Buckets {
		if b.Start == "2026-06-17T10:00:00Z" {
			found = true
			assert.Equal(t, "2026-06-17T11:00:00Z", b.End)
			assert.InDelta(t, 5.0, b.AgentMinutes, 1e-9,
				"mid-range hourly bucket is populated")
		}
	}
	assert.True(t, found, "the 2026-06-17T10:00 hourly bucket must be present")
}

// TestGetActivityReport_UsageDedupSubSecondOrder confirms the SQLite usage
// stream is ordered by the PARSED instant, not the RFC3339 text. A
// resumed/forked pair shares one (claude_message_id, claude_request_id) dedup
// key in the same second: one whole-second instant ("...00Z", 500 output
// tokens) and one fractional ("...00.123Z", 9000). Lexically "...00.123Z"
// sorts before "...00Z" ('.' < 'Z'), so a TEXT sort would keep the 9000 row;
// chronologically the whole-second row is first. First-seen-wins dedup must
// keep the 500 row, matching PostgreSQL/DuckDB which order on the parsed time.
func TestGetActivityReport_UsageDedupSubSecondOrder(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}), "UpsertModelPricing")

	insertSession(t, d, "earlier", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:30:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "earlier", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp:       "2026-06-16T10:30:00Z",
		Model:           "claude-sonnet-4-20250514",
		ClaudeMessageID: "m-dup", ClaudeRequestID: "r-dup",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})
	insertSession(t, d, "later", "proj2", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:00Z")
		s.EndedAt = Ptr("2026-06-16T10:30:00Z")
	})
	insertMessages(t, d, Message{
		SessionID: "later", Ordinal: 0, Role: "assistant", Content: "x",
		Timestamp:       "2026-06-16T10:30:00.123Z",
		Model:           "claude-sonnet-4-20250514",
		ClaudeMessageID: "m-dup", ClaudeRequestID: "r-dup",
		TokenUsage: json.RawMessage(`{"input_tokens":1000,"output_tokens":9000}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens,
		"first-seen dedup keeps the chronologically earlier whole-second row")
}
