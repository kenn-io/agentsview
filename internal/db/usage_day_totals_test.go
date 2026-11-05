//go:build fts5

package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

// Compare persisted totals with an independent GROUP BY, including dimensions
// and provenance counts. Removing either trigger breaks this invariant.
func assertUsageDayTotals(t *testing.T, cache *usageCache) {
	t.Helper()
	const query = `WITH expected AS (
        SELECT i.timezone_id AS timezone_id, r.local_date AS local_date, i.project AS project, i.baked_agent AS agent, i.machine AS machine, i.git_branch AS git_branch, i.is_automated AS is_automated, i.user_message_count AS user_message_count, r.reported_model AS reported_model, r.provider_id AS provider_id, r.rate_hash AS rate_hash, r.band_threshold AS band_threshold, COUNT(*) AS row_count, SUM(r.input_tokens) AS input_tokens, SUM(r.output_tokens) AS output_tokens, SUM(r.reasoning_tokens) AS reasoning_tokens, SUM(r.cache_creation_tokens) AS cache_creation_tokens, SUM(r.cache_read_tokens) AS cache_read_tokens, SUM(r.estimated_cost_microdollars) AS estimated_cost_microdollars, SUM(r.savings_microdollars) AS savings_microdollars, SUM(r.computed_request_count) AS computed_request_count, SUM(r.computed_aggregate_count) AS computed_aggregate_count, SUM(r.reported_count) AS reported_count, SUM(r.base_request_count) AS base_request_count
        FROM usage_daily_rollups r JOIN usage_rollup_installs i ON i.id=r.rollup_install_id
        WHERE i.has_authoritative=0 GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12
    ), missing AS (SELECT timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold, row_count, input_tokens, output_tokens, reasoning_tokens, cache_creation_tokens, cache_read_tokens, estimated_cost_microdollars, savings_microdollars, computed_request_count, computed_aggregate_count, reported_count, base_request_count FROM expected EXCEPT SELECT timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold, row_count, input_tokens, output_tokens, reasoning_tokens, cache_creation_tokens, cache_read_tokens, estimated_cost_microdollars, savings_microdollars, computed_request_count, computed_aggregate_count, reported_count, base_request_count FROM usage_day_totals),
    extra AS (SELECT timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold, row_count, input_tokens, output_tokens, reasoning_tokens, cache_creation_tokens, cache_read_tokens, estimated_cost_microdollars, savings_microdollars, computed_request_count, computed_aggregate_count, reported_count, base_request_count FROM usage_day_totals EXCEPT SELECT timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold, row_count, input_tokens, output_tokens, reasoning_tokens, cache_creation_tokens, cache_read_tokens, estimated_cost_microdollars, savings_microdollars, computed_request_count, computed_aggregate_count, reported_count, base_request_count FROM expected)
    SELECT (SELECT COUNT(*) FROM missing)+(SELECT COUNT(*) FROM extra)+(SELECT COUNT(*) FROM usage_day_totals WHERE row_count<=0)`
	var differences int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), query).Scan(&differences))
	assert.Zero(t, differences)
}

func TestUsageDayTotalsFiltersMatchLegacy(t *testing.T) {
	database := seedUsageDayTotalsPage(t, 32)
	filter := UsageFilter{From: "2026-08-03", To: "2026-08-10", Timezone: "UTC"}
	cache := warmJournaledUsage(t, database, filter)
	for _, tc := range []struct {
		name   string
		change func(*UsageFilter)
	}{
		{"unfiltered", func(f *UsageFilter) {}},
		{"breakdowns", func(f *UsageFilter) { f.Breakdowns = true }},
		{"project", func(f *UsageFilter) { f.Project = "project-0" }},
		{"project labels", func(f *UsageFilter) { f.ProjectLabels = []string{"project-0", "project-1"} }},
		{"agent", func(f *UsageFilter) { f.Agent = "claude" }},
		{"machine", func(f *UsageFilter) { f.Machine = "machine-1" }},
		{"branch", func(f *UsageFilter) { f.GitBranch = EncodeBranchFilterToken("project-0", "branch-0") }},
		{"model", func(f *UsageFilter) { f.Model = "journal-model" }},
		{"exclude project", func(f *UsageFilter) { f.ExcludeProject = "project-0" }},
		{"exclude labels", func(f *UsageFilter) { f.ExcludeProjectLabels = []string{"project-0"} }},
		{"exclude agent", func(f *UsageFilter) { f.ExcludeAgent = "claude" }},
		{"exclude model", func(f *UsageFilter) { f.ExcludeModel = "journal-model" }},
		{"human", func(f *UsageFilter) { f.AutomatedScope = "human" }},
		{"automated", func(f *UsageFilter) { f.AutomatedScope = "automated" }},
		{"exclude automated", func(f *UsageFilter) { f.ExcludeAutomated = true }},
		{"exclude one shot", func(f *UsageFilter) { f.ExcludeOneShot = true }},
		{"human multiple messages", func(f *UsageFilter) { f.ExcludeOneShot = true; f.AutomatedScope = "human" }},
		{"minimum messages", func(f *UsageFilter) { f.MinUserMessages = 3 }},
		{"skip counts", func(f *UsageFilter) { f.SkipSessionCounts = true }},
		{"all history", func(f *UsageFilter) { f.From = ""; f.To = "" }},
		{"active since", func(f *UsageFilter) { f.ActiveSince = "2026-08-06T00:00:00Z" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := filter
			tc.change(&f)
			want, err := database.getDailyUsageLegacy(t.Context(), f)
			require.NoError(t, err)
			got, err := database.GetDailyUsage(t.Context(), f)
			require.NoError(t, err)
			assert.Equal(t, mustUsageJSON(t, want), mustUsageJSON(t, got))
		})
	}
	assertUsageDayTotals(t, cache)
	prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindDayTotals, true, nil)
	require.NoError(t, err)
	require.True(t, prepared.journaled)
	assert.Empty(t, prepared.snapshot.Sessions, "a warm totals read must skip archive window metadata")
	assert.Empty(t, prepared.snapshot.Versions)
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	want, err := database.getDailyUsageLegacy(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, mustUsageJSON(t, want), mustUsageJSON(t, got))
	for _, tc := range []struct {
		filter UsageFilter
		owners []string
	}{
		{UsageFilter{From: "2026-08-05", To: "2026-08-05", Timezone: "UTC"}, nil},
		{UsageFilter{From: "2026-08-03", To: "2026-08-03", Timezone: "UTC", Agent: "codex"}, []string{"duplicate-a", "duplicate-b"}},
	} {
		prepared, err := database.prepareUsageWindow(t.Context(), tc.filter, usageQueryKindDayTotals, true, nil)
		require.NoError(t, err)
		conn, err := cache.db.Conn(t.Context())
		require.NoError(t, err)
		identity := usageTimezoneIdentityFor(prepared.snapshot.location, prepared.snapshot.Intervals)
		timezoneID, err := verifyPreparedUsageRollup(t.Context(), conn, identity.Key, prepared)
		require.NoError(t, err)
		_, err = readUsageDayTotals(t.Context(), conn, timezoneID, &prepared.snapshot, tc.filter)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
		var owners []string
		for _, session := range prepared.snapshot.Sessions {
			owners = append(owners, session.ID)
		}
		assert.ElementsMatch(t, tc.owners, owners, "load window exception owners, leaving unrelated authoritative sessions unloaded")
	}
}

func TestUsageDayTotalsInvalidation(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{Timezone: "UTC", Breakdowns: true}
	cache := warmJournaledUsage(t, database, filter)
	assertUsageDayTotals(t, cache)
	for _, name := range []string{"append", "project", "dedup peer", "price", "install delete", "hard delete"} {
		t.Run(name, func(t *testing.T) {
			var err error
			switch name {
			case "append":
				err = database.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-11T01:00:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":50}`), SourceUUID: "shared-source"}})
			case "project":
				_, err = database.getWriter().ExecContext(t.Context(), `UPDATE sessions SET project='changed' WHERE id='session'`)
			case "dedup peer":
				insertSession(t, database, "peer", "project", func(s *Session) { s.Agent = "codex"; s.StartedAt = new("2026-08-11T02:00:00Z") })
				err = database.InsertMessages(t.Context(), []Message{{SessionID: "peer", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-11T02:00:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":80}`), SourceUUID: "shared-source"}})
			case "price":
				err = database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 2000000}}})
			case "install delete":
				_, err = cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs WHERE session_id='session'`)
				require.NoError(t, err)
				assertUsageDayTotals(t, cache)
			case "hard delete":
				err = database.DeleteSession(t.Context(), "session")
			}
			require.NoError(t, err)
			got, err := database.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			want, err := database.getDailyUsageLegacy(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, mustUsageJSON(t, want), mustUsageJSON(t, got))
			assertUsageDayTotals(t, cache)
		})
	}
}
