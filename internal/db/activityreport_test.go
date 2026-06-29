package db

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
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

func TestGetActivityReport_UsageDedupFallsBackToSourceUUID(t *testing.T) {
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
		SessionID:       "earlier",
		Ordinal:         0,
		Role:            "assistant",
		Content:         "x",
		Timestamp:       "2026-06-16T10:30:00Z",
		Model:           "claude-sonnet-4-20250514",
		ClaudeMessageID: "m-dup",
		ClaudeRequestID: "",
		SourceUUID:      "src-dup",
		TokenUsage:      json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`),
	})
	insertSession(t, d, "later", "proj2", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-16T10:30:01Z")
		s.EndedAt = Ptr("2026-06-16T10:30:01Z")
	})
	insertMessages(t, d, Message{
		SessionID:       "later",
		Ordinal:         0,
		Role:            "assistant",
		Content:         "x",
		Timestamp:       "2026-06-16T10:30:01Z",
		Model:           "claude-sonnet-4-20250514",
		ClaudeMessageID: "m-dup",
		ClaudeRequestID: "",
		SourceUUID:      "src-dup",
		TokenUsage:      json.RawMessage(`{"input_tokens":1000,"output_tokens":900}`),
	})

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens,
		"incomplete Claude pairs fall back to source_uuid dedup in activity reports")
}

// TestGetActivityReport_TitleSkipsEmptyDisplayName confirms the Title fallback
// null-checks each candidate independently: an empty (non-NULL) display_name
// must not mask a real session_name. A nested COALESCE(display_name,
// session_name) would return an empty string and be NULLIF'd away, wrongly
// skipping to first_message. RenameSession stores a literal empty string (only
// nil clears to NULL), so this reproduces a session renamed to "" that still
// has a session_name.
func TestGetActivityReport_TitleSkipsEmptyDisplayName(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s", "proj", func(s *Session) {
		s.Agent = "claude"
		s.SessionName = Ptr("real-session-name")
		s.FirstMessage = Ptr("first message text")
		s.StartedAt = Ptr("2026-06-16T10:00:00Z")
		s.EndedAt = Ptr("2026-06-16T10:02:00Z")
	})
	require.NoError(t, d.RenameSession("s", Ptr("")))
	seedMessage(t, d, "s", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "s", 2, "assistant", "2026-06-16T10:02:00Z", "opus")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	require.Len(t, r.BySession, 1)
	assert.Equal(t, "real-session-name", r.BySession[0].Title,
		"empty display_name must not mask the real session_name")
}

// TestGetActivityReport_OpenSessionWithInRangeMessageIncluded confirms a
// still-open session (no ended_at) that started before the range but has a
// message inside it is not dropped. The effective-end fallback uses the
// session's latest message timestamp, not started_at, so the overlap
// predicate sees the in-range activity. Without the fix, ended_at falls back
// to the pre-range started_at and the session vanishes from the report.
func TestGetActivityReport_OpenSessionWithInRangeMessageIncluded(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Started the day before and never closed (no ended_at), active in-range.
	insertSession(t, d, "open", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = Ptr("2026-06-15T23:00:00Z")
	})
	seedMessage(t, d, "open", 1, "user", "2026-06-16T10:00:00Z", "")
	seedMessage(t, d, "open", 2, "assistant", "2026-06-16T10:02:00Z", "opus")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	ids := reportSessionIDs(r.BySession)
	assert.Contains(t, ids, "open",
		"open session active in-range must not be dropped by the started_at fallback")
	assert.Equal(t, 1, r.Totals.Sessions)
}

// TestGetActivityReport_AutomationFilterAndSessionSplit confirms the
// AnalyticsFilter automation class selects the right sessions and that the
// Totals carry the automated/interactive session-count split. "all" keeps
// both classes; ExcludeAutomated keeps only interactive sessions;
// ExcludeInteractive (the mirror predicate) keeps only automated ones.
func TestGetActivityReport_AutomationFilterAndSessionSplit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Two automated (roborev-style) sessions and one interactive, all timed
	// on 2026-06-16 so each is a candidate for that day's report. Automated
	// sessions are classified the way the sync path does it: a single-turn
	// session whose first message matches a known automated prompt prefix.
	for _, id := range []string{"auto1", "auto2"} {
		start := "2026-06-16T10:00:00Z"
		end := "2026-06-16T10:02:00Z"
		insertSession(t, d, id, "proj1", func(s *Session) {
			s.Agent = "claude"
			s.FirstMessage = Ptr("You are a code reviewer.")
			s.UserMessageCount = 1
			s.StartedAt = Ptr(start)
			s.EndedAt = Ptr(end)
		})
		seedMessage(t, d, id, 1, "user", start, "")
		seedMessage(t, d, id, 2, "assistant", end, "opus")
	}
	insertSession(t, d, "human", "proj2", func(s *Session) {
		s.Agent = "codex"
		s.StartedAt = Ptr("2026-06-16T12:00:00Z")
		s.EndedAt = Ptr("2026-06-16T12:02:00Z")
	})
	seedMessage(t, d, "human", 1, "user", "2026-06-16T12:00:00Z", "")
	seedMessage(t, d, "human", 2, "assistant", "2026-06-16T12:02:00Z", "gpt5")

	tests := []struct {
		name            string
		filter          AnalyticsFilter
		wantAutomated   int
		wantInteractive int
		wantIDs         []string
	}{
		{
			name:            "all keeps both classes",
			filter:          AnalyticsFilter{Timezone: "UTC"},
			wantAutomated:   2,
			wantInteractive: 1,
			wantIDs:         []string{"auto1", "auto2", "human"},
		},
		{
			name:            "exclude automated keeps interactive only",
			filter:          AnalyticsFilter{Timezone: "UTC", ExcludeAutomated: true},
			wantAutomated:   0,
			wantInteractive: 1,
			wantIDs:         []string{"human"},
		},
		{
			name:            "exclude interactive keeps automated only",
			filter:          AnalyticsFilter{Timezone: "UTC", ExcludeInteractive: true},
			wantAutomated:   2,
			wantInteractive: 0,
			wantIDs:         []string{"auto1", "auto2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := d.GetActivityReport(ctx, tc.filter,
				dayQuery(t, "2026-06-16", "UTC"))
			require.NoError(t, err)
			assert.Equal(t, len(tc.wantIDs), r.Totals.Sessions)
			assert.Equal(t, tc.wantAutomated, r.Totals.AutomatedSessions)
			assert.Equal(t, tc.wantInteractive, r.Totals.InteractiveSessions)
			ids := reportSessionIDs(r.BySession)
			require.Len(t, ids, len(tc.wantIDs))
			for _, id := range tc.wantIDs {
				assert.Contains(t, ids, id)
			}
		})
	}
}

// forceReaderVarLimit pins the reader pool to a single connection and lowers
// its SQLITE_LIMIT_VARIABLE_NUMBER to mimic older SQLite builds, whose limit is
// the documented 999 rather than the modern default (32766). Every read through
// d.getReader() then reuses this one constrained connection, so a query that
// binds too many variables fails exactly as it would on those builds.
func forceReaderVarLimit(t *testing.T, d *DB, limit int) {
	t.Helper()
	reader := d.rawReader()
	reader.SetMaxOpenConns(1)
	reader.SetMaxIdleConns(1)
	conn, err := reader.Conn(context.Background())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	require.NoError(t, conn.Raw(func(dc any) error {
		sc, ok := dc.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("reader conn is %T, want *sqlite3.SQLiteConn", dc)
		}
		sc.SetLimit(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER, limit)
		return nil
	}))
}

// TestGetActivityReport_ManySessionsWithinSQLiteVarLimit reproduces the older
// SQLite 999-variable limit on the reader pool, then builds a report whose
// candidate set exceeds it. The usage fetch binds each id chunk twice (the
// message-where and usage-event-where subqueries) plus two time bounds, so a
// generic maxSQLVars chunk would emit 2*maxSQLVars+2 = 1002 variables and fail
// on such builds. The fetch must instead chunk small enough to stay within the
// limit while still aggregating usage across every chunk.
func TestGetActivityReport_ManySessionsWithinSQLiteVarLimit(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// More candidate sessions than maxSQLVars, so the usage fetch must chunk.
	const n = maxSQLVars + 1
	writes := make([]SessionBatchWrite, 0, n)
	for i := range n {
		sid := fmt.Sprintf("s%04d", i)
		writes = append(writes, SessionBatchWrite{
			Session: Session{
				ID:           sid,
				Project:      "proj1",
				Machine:      defaultMachine,
				Agent:        "claude",
				MessageCount: 1,
				StartedAt:    Ptr("2026-06-16T10:00:00Z"),
				EndedAt:      Ptr("2026-06-16T10:01:00Z"),
				FirstMessage: Ptr("x"),
			},
			Messages: []Message{{
				SessionID:  sid,
				Ordinal:    0,
				Role:       "assistant",
				Content:    "x",
				Timestamp:  "2026-06-16T10:00:00Z",
				Model:      "claude-sonnet-4-20250514",
				TokenUsage: json.RawMessage(`{"input_tokens":100,"output_tokens":10}`),
			}},
		})
	}
	result, err := d.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	require.Equal(t, n, result.WrittenSessions)
	require.Equal(t, n, result.WrittenMessages)

	forceReaderVarLimit(t, d, 999)

	// Guard: prove the lowered limit is live on the pool, so a setup that
	// failed to constrain it cannot mask the regression checked below.
	overLimitPh, overLimitArgs := inPlaceholders(make([]string, 1001))
	_, probeErr := d.getReader().QueryContext(
		ctx, "SELECT 1 WHERE '' IN "+overLimitPh, overLimitArgs...)
	require.Error(t, probeErr, "reader variable limit was not constrained")

	r, err := d.GetActivityReport(ctx, AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-06-16", "UTC"))
	require.NoError(t, err)
	assert.Len(t, reportSessionIDs(r.BySession), n,
		"every candidate session survives id chunking")
	assert.Positive(t, r.Totals.OutputTokens,
		"usage aggregated across all id chunks")
}
