//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// duckDayQuery resolves a single-day "day" Query for date/tz against a
// fixed far-future now, so the candidate range is the full local day and
// the report is never partial regardless of the wall clock.
func duckDayQuery(t *testing.T, date, tz string) activity.Query {
	t.Helper()
	now, err := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	require.NoError(t, err)
	q, err := activity.ResolveQuery(
		activity.QueryInput{Preset: "day", Date: date, Timezone: tz}, now)
	require.NoError(t, err)
	return q
}

// activityReportStore seeds the given writes into a fresh local SQLite DB,
// pushes them into DuckDB, and returns a read-only DuckDB store, mirroring
// newSyncedStore's sync path.
func activityReportStore(
	t *testing.T, writes []db.SessionBatchWrite, pricing []db.ModelPricing,
) *Store {
	t.Helper()
	ctx := context.Background()
	local := newLocalDB(t)
	if len(pricing) > 0 {
		require.NoError(t, local.UpsertModelPricing(pricing))
	}
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	return NewStoreFromDB(syncer.DB())
}

func TestDuckGetActivityReportBasicConcurrency(t *testing.T) {
	ctx := context.Background()
	// Two overlapping sessions on 2026-06-14 (UTC), each two timestamped
	// messages, mirroring the SQLite and PostgreSQL parity fixtures.
	aSession := syncSession("a", "proj1", "alpha first", "2026-06-14T10:00:00.000Z", 2)
	aSession.Agent = "claude"
	bSession := syncSession("b", "proj2", "beta first", "2026-06-14T10:01:00.000Z", 2)
	bSession.Agent = "codex"
	writes := []db.SessionBatchWrite{
		{
			Session: aSession,
			Messages: []db.Message{
				syncMessage("a", 0, "user", "u", "2026-06-14T10:00:00.000Z"),
				syncMessage("a", 1, "assistant", "x", "2026-06-14T10:02:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: bSession,
			Messages: []db.Message{
				syncMessage("b", 0, "user", "u", "2026-06-14T10:01:00.000Z"),
				syncMessage("b", 1, "assistant", "x", "2026-06-14T10:03:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	store := activityReportStore(t, writes, nil)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 2, r.Peak.Agents)
	assert.Equal(t, 2, r.Totals.Sessions)
	assert.GreaterOrEqual(t, len(r.ByAgent), 2)
}

func TestDuckGetActivityReportUsageCostAndTokens(t *testing.T) {
	ctx := context.Background()
	sess := syncSession("s1", "proj1", "first", "2026-06-14T10:30:00.000Z", 1)
	sess.Agent = "claude"
	// Override the default token usage to a known input/output split so the
	// cost is deterministic.
	msg := syncMessage("s1", 0, "assistant", "x", "2026-06-14T10:30:00.000Z")
	msg.Model = "claude-sonnet-4-20250514"
	msg.TokenUsage = json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500}`)
	msg.OutputTokens = 500
	writes := []db.SessionBatchWrite{{
		Session:         sess,
		Messages:        []db.Message{msg},
		DataVersion:     1,
		ReplaceMessages: true,
	}}
	pricing := []db.ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 1, r.Totals.Sessions)
	assert.Equal(t, 500, r.Totals.OutputTokens)
	// Cost = (1000*3 + 500*15) / 1e6 = 0.0105
	assert.InDelta(t, 0.0105, r.Totals.Cost, 1e-9)
}

func TestDuckGetActivityReportPreservesSessionSummaryUsageEventTokens(t *testing.T) {
	ctx := context.Background()
	rawInput := db.MaxPlausibleTokens + 250_000
	rawOutput := db.MaxPlausibleTokens + 500_000
	sessionID := "summary-activity"
	sess := syncSession(sessionID, "proj1", "first", "2026-06-14T10:30:00.000Z", 1)
	sess.Agent = "hermes"
	sess.TotalOutputTokens = rawOutput
	sess.PeakContextTokens = rawInput
	sess.HasTotalOutputTokens = true
	sess.HasPeakContextTokens = true
	msg := syncMessage(sessionID, 0, "user", "first", "2026-06-14T10:30:00.000Z")
	msg.Model = ""
	msg.TokenUsage = nil
	writes := []db.SessionBatchWrite{{
		Session:  sess,
		Messages: []db.Message{msg},
		UsageEvents: []db.UsageEvent{{
			Source:       "session",
			Model:        "summary-model",
			InputTokens:  rawInput,
			OutputTokens: rawOutput,
			OccurredAt:   "2026-06-14T10:30:00.000Z",
			DedupKey:     "summary",
		}},
		DataVersion:     1,
		ReplaceMessages: true,
	}}
	pricing := []db.ModelPricing{{
		ModelPattern:  "summary-model",
		InputPerMTok:  1,
		OutputPerMTok: 2,
	}}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, rawOutput, r.Totals.OutputTokens)
	wantCost := (float64(rawInput)*1 + float64(rawOutput)*2) / 1_000_000
	assert.InDelta(t, wantCost, r.Totals.Cost, 1e-9)
}

// TestDuckGetActivityReportExcludesIneligibleUsage confirms the DuckDB
// usage union (the one backend that inlines its own usage CTE rather
// than sharing dailyUsageRowsSQLWithWhere) applies the same eligibility
// filters as GetDailyUsage: a synthetic-model message carrying real
// token_usage must not inflate the day totals. Mirrors the PostgreSQL
// TestPGGetActivityReportExcludesIneligibleUsage.
func TestDuckGetActivityReportExcludesIneligibleUsage(t *testing.T) {
	ctx := context.Background()
	sess := syncSession("s1", "proj1", "first", "2026-06-14T10:30:00.000Z", 2)
	sess.Agent = "claude"
	end := "2026-06-14T10:31:00.000Z"
	sess.EndedAt = &end

	eligible := syncMessage("s1", 0, "assistant", "x", "2026-06-14T10:30:00.000Z")
	eligible.Model = "claude-sonnet-4-20250514"
	eligible.TokenUsage = json.RawMessage(
		`{"input_tokens":1000,"output_tokens":500}`)
	eligible.OutputTokens = 500
	// Ineligible: a synthetic-model message carrying real token_usage. The
	// usage CTE drops m.model == '<synthetic>', so these tokens must NOT leak
	// into the day totals even though the blob is non-empty.
	synthetic := syncMessage("s1", 1, "assistant", "y", "2026-06-14T10:31:00.000Z")
	synthetic.Model = "<synthetic>"
	synthetic.TokenUsage = json.RawMessage(
		`{"input_tokens":9000,"output_tokens":7000}`)
	synthetic.OutputTokens = 7000

	writes := []db.SessionBatchWrite{{
		Session:         sess,
		Messages:        []db.Message{eligible, synthetic},
		DataVersion:     1,
		ReplaceMessages: true,
	}}
	pricing := []db.ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens, "synthetic message excluded")
	// Cost = (1000*3 + 500*15) / 1e6 = 0.0105
	assert.InDelta(t, 0.0105, r.Totals.Cost, 1e-9)
}

// TestDuckGetActivityReportPriorDayWithinPadExcluded confirms the candidate
// window uses the EXACT local day, not the +/-14h padded bounds: a
// session that began and ended on the prior day but lands inside the pad
// must NOT appear as an untimed session in the target day's report.
func TestDuckGetActivityReportPriorDayWithinPadExcluded(t *testing.T) {
	ctx := context.Background()
	today := syncSession("today", "proj1", "today first", "2026-06-14T10:00:00.000Z", 2)
	today.Agent = "claude"
	prior := syncSession("prior", "proj2", "prior first", "2026-06-13T12:00:00.000Z", 1)
	prior.Agent = "codex"
	priorEnd := "2026-06-13T12:05:00.000Z"
	prior.EndedAt = &priorEnd
	writes := []db.SessionBatchWrite{
		{
			Session: today,
			Messages: []db.Message{
				syncMessage("today", 0, "user", "u", "2026-06-14T10:00:00.000Z"),
				syncMessage("today", 1, "assistant", "x", "2026-06-14T10:02:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: prior,
			Messages: []db.Message{
				syncMessage("prior", 0, "user", "u", "2026-06-13T12:00:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	}
	store := activityReportStore(t, writes, nil)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	ids := make(map[string]struct{}, len(r.BySession))
	for _, s := range r.BySession {
		ids[s.SessionID] = struct{}{}
	}
	assert.Contains(t, ids, "today")
	assert.NotContains(t, ids, "prior", "prior-day session must not leak in")
	assert.Equal(t, 1, r.Totals.Sessions)
	assert.Equal(t, 0, r.Totals.UntimedSessions)
}

// TestDuckGetActivityReportOpenSessionWithInRangeMessageIncluded confirms a
// still-open session (no ended_at) that started before the range but has a
// message inside it is not dropped. The effective-end fallback uses the
// session's latest message timestamp, not started_at, matching SQLite and
// PostgreSQL. Mirrors the SQLite
// TestGetActivityReport_OpenSessionWithInRangeMessageIncluded.
func TestDuckGetActivityReportOpenSessionWithInRangeMessageIncluded(t *testing.T) {
	ctx := context.Background()
	// Started the day before and never closed (ended_at nil), active in-range.
	open := syncSession("open", "proj1", "open first", "2026-06-13T23:00:00.000Z", 2)
	open.Agent = "claude"
	open.EndedAt = nil
	writes := []db.SessionBatchWrite{{
		Session: open,
		Messages: []db.Message{
			syncMessage("open", 0, "user", "u", "2026-06-14T10:00:00.000Z"),
			syncMessage("open", 1, "assistant", "x", "2026-06-14T10:02:00.000Z"),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}}
	store := activityReportStore(t, writes, nil)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	ids := make(map[string]struct{}, len(r.BySession))
	for _, s := range r.BySession {
		ids[s.SessionID] = struct{}{}
	}
	assert.Contains(t, ids, "open",
		"open session active in-range must not be dropped by the started_at fallback")
	assert.Equal(t, 1, r.Totals.Sessions)
}

// TestDuckGetActivityReportUsageDedupSubSecondOrder confirms DuckDB orders the
// usage stream by the parsed instant so first-seen-wins dedup keeps the
// chronologically earlier row when two rows share a dedup key in the same
// second -- one whole-second ("...00Z"), one fractional ("...00.123Z"). DuckDB
// already sorts on the parsed time (not the formatted text), so this locks in
// that cross-backend behavior, matching the SQLite
// TestGetActivityReport_UsageDedupSubSecondOrder.
func TestDuckGetActivityReportUsageDedupSubSecondOrder(t *testing.T) {
	ctx := context.Background()
	pricing := []db.ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}

	// A resumed/forked pair shares one (claude_message_id, claude_request_id)
	// across two sessions: the earlier whole-second instant carries 500 output
	// tokens, the later fractional instant 9000. Dedup must keep the 500 row.
	earlier := syncSession("earlier", "proj1", "first", "2026-06-14T10:30:00Z", 1)
	earlierMsg := syncMessage("earlier", 0, "assistant", "x", "2026-06-14T10:30:00Z")
	earlierMsg.Model = "claude-sonnet-4-20250514"
	earlierMsg.ClaudeMessageID = "dup-m"
	earlierMsg.ClaudeRequestID = "dup-r"
	earlierMsg.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	earlierMsg.OutputTokens = 500

	later := syncSession("later", "proj2", "first", "2026-06-14T10:30:00.123Z", 1)
	laterMsg := syncMessage("later", 0, "assistant", "x", "2026-06-14T10:30:00.123Z")
	laterMsg.Model = "claude-sonnet-4-20250514"
	laterMsg.ClaudeMessageID = "dup-m"
	laterMsg.ClaudeRequestID = "dup-r"
	laterMsg.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":9000}`)
	laterMsg.OutputTokens = 9000

	writes := []db.SessionBatchWrite{
		{Session: earlier, Messages: []db.Message{earlierMsg},
			DataVersion: 1, ReplaceMessages: true},
		{Session: later, Messages: []db.Message{laterMsg},
			DataVersion: 1, ReplaceMessages: true},
	}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens,
		"first-seen dedup keeps the chronologically earlier whole-second row")
}

func TestDuckGetActivityReportUsageDedupFallsBackToSourceUUID(t *testing.T) {
	ctx := context.Background()
	pricing := []db.ModelPricing{{
		ModelPattern:  "claude-sonnet-4-20250514",
		InputPerMTok:  3.0,
		OutputPerMTok: 15.0,
	}}

	earlier := syncSession("earlier", "proj1", "first", "2026-06-14T10:30:00Z", 1)
	earlier.Agent = "claude"
	earlierMsg := syncMessage("earlier", 0, "assistant", "x", "2026-06-14T10:30:00Z")
	earlierMsg.Model = "claude-sonnet-4-20250514"
	earlierMsg.ClaudeMessageID = "dup-m"
	earlierMsg.SourceUUID = "src-dup"
	earlierMsg.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	earlierMsg.OutputTokens = 500

	later := syncSession("later", "proj2", "first", "2026-06-14T10:30:01Z", 1)
	later.Agent = "claude"
	laterMsg := syncMessage("later", 0, "assistant", "x", "2026-06-14T10:30:01Z")
	laterMsg.Model = "claude-sonnet-4-20250514"
	laterMsg.ClaudeMessageID = "dup-m"
	laterMsg.SourceUUID = "src-dup"
	laterMsg.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":900}`)
	laterMsg.OutputTokens = 900

	writes := []db.SessionBatchWrite{
		{Session: earlier, Messages: []db.Message{earlierMsg},
			DataVersion: 1, ReplaceMessages: true},
		{Session: later, Messages: []db.Message{laterMsg},
			DataVersion: 1, ReplaceMessages: true},
	}
	store := activityReportStore(t, writes, pricing)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, 500, r.Totals.OutputTokens,
		"incomplete Claude pairs fall back to source_uuid dedup in activity reports")
}

// TestDuckGetActivityReportZeroCostKeepsPrimaryModel confirms a usage-only
// (untimed) session whose known-model usage carries zero cost still reports
// that model as primary through the DuckDB path, guarding the shared zero-cost
// fallback end-to-end. Mirrors the aggregator unit test
// TestAggregate_UsageOnlySessionZeroCostKeepsPrimaryModel.
func TestDuckGetActivityReportZeroCostKeepsPrimaryModel(t *testing.T) {
	ctx := context.Background()
	sess := syncSession("u", "proj1", "first", "2026-06-14T10:30:00Z", 1)
	msg := syncMessage("u", 0, "assistant", "x", "2026-06-14T10:30:00Z")
	// Known model, unpriced and zero tokens -> a usage row with zero cost.
	msg.Model = "model-x"
	msg.TokenUsage = json.RawMessage(`{"input_tokens":0,"output_tokens":0}`)
	msg.OutputTokens = 0
	writes := []db.SessionBatchWrite{{
		Session: sess, Messages: []db.Message{msg},
		DataVersion: 1, ReplaceMessages: true,
	}}
	store := activityReportStore(t, writes, nil)

	r, err := store.GetActivityReport(
		ctx, db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-06-14", "UTC"))
	require.NoError(t, err)
	require.Len(t, r.BySession, 1)
	assert.Equal(t, "model-x", r.BySession[0].PrimaryModel,
		"zero-cost usage must still report its known model as primary")
}

// TestDuckGetActivityReportAutomationFilterAndSessionSplit confirms the shared
// AnalyticsFilter automation class is honored through the DuckDB analytics
// WHERE builder and that the Totals session-count split survives the sync into
// DuckDB. Mirrors the SQLite
// TestGetActivityReport_AutomationFilterAndSessionSplit.
func TestDuckGetActivityReportAutomationFilterAndSessionSplit(t *testing.T) {
	ctx := context.Background()

	// The sync path (WriteSessionBatchAtomic) classifies is_automated from the
	// transcript: a single-turn session whose first user message matches a
	// known automated (roborev) prompt prefix. Setting the struct flag alone
	// would be overridden by updateSessionAutomationFromMessagesTx, so the
	// automated sessions carry an automated first user message and a single
	// user turn, exactly as a real roborev review session does.
	auto1 := syncSession("auto1", "proj1", "You are a code reviewer.", "2026-06-14T10:00:00.000Z", 2)
	auto1.Agent = "claude"
	auto2 := syncSession("auto2", "proj1", "You are a code reviewer.", "2026-06-14T11:00:00.000Z", 2)
	auto2.Agent = "claude"
	human := syncSession("human", "proj2", "human first", "2026-06-14T12:00:00.000Z", 2)
	human.Agent = "codex"

	writes := []db.SessionBatchWrite{
		{
			Session: auto1,
			Messages: []db.Message{
				syncMessage("auto1", 0, "user", "You are a code reviewer.", "2026-06-14T10:00:00.000Z"),
				syncMessage("auto1", 1, "assistant", "x", "2026-06-14T10:02:00.000Z"),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: auto2,
			Messages: []db.Message{
				syncMessage("auto2", 0, "user", "You are a code reviewer.", "2026-06-14T11:00:00.000Z"),
				syncMessage("auto2", 1, "assistant", "x", "2026-06-14T11:02:00.000Z"),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: human,
			Messages: []db.Message{
				syncMessage("human", 0, "user", "u", "2026-06-14T12:00:00.000Z"),
				syncMessage("human", 1, "assistant", "x", "2026-06-14T12:02:00.000Z"),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
	}
	store := activityReportStore(t, writes, nil)

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
				duckDayQuery(t, "2026-06-14", "UTC"))
			require.NoError(t, err)
			assert.Equal(t, len(tc.wantIDs), r.Totals.Sessions)
			assert.Equal(t, tc.wantAutomated, r.Totals.AutomatedSessions)
			assert.Equal(t, tc.wantInteractive, r.Totals.InteractiveSessions)
			ids := make(map[string]struct{}, len(r.BySession))
			for _, s := range r.BySession {
				ids[s.SessionID] = struct{}{}
			}
			require.Len(t, ids, len(tc.wantIDs))
			for _, id := range tc.wantIDs {
				assert.Contains(t, ids, id)
			}
		})
	}
}
