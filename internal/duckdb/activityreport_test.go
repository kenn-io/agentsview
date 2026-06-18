//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"encoding/json"
	"path/filepath"
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
	syncer := newTestSync(t,
		filepath.Join(t.TempDir(), "daily-report.duckdb"),
		local, SyncOptions{})
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
