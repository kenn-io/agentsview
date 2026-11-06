//go:build fts5

package db

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

// Compare persisted totals with an independent GROUP BY, including dimensions
// and provenance counts. Removing either trigger breaks this invariant.
func assertUsageDayTotals(t *testing.T, cache *usageCache) {
	t.Helper()
	const query = `WITH expected AS (
        SELECT i.timezone_id AS timezone_id, r.local_date AS local_date, i.project AS project, i.baked_agent AS agent, i.machine AS machine, i.git_branch AS git_branch, i.is_automated AS is_automated, i.user_message_count AS user_message_count, r.reported_model AS reported_model, r.provider_id AS provider_id, r.rate_hash AS rate_hash, r.band_threshold AS band_threshold, COUNT(*) AS row_count, SUM(r.input_tokens) AS input_tokens, SUM(r.output_tokens) AS output_tokens, SUM(r.cache_creation_tokens) AS cache_creation_tokens, SUM(r.cache_read_tokens) AS cache_read_tokens, SUM(r.estimated_cost_microdollars) AS estimated_cost_microdollars, SUM(r.savings_microdollars) AS savings_microdollars, SUM(r.computed_request_count) AS computed_request_count, SUM(r.computed_aggregate_count) AS computed_aggregate_count, SUM(r.reported_count) AS reported_count, SUM(r.base_request_count) AS base_request_count
        FROM usage_daily_rollups r JOIN usage_rollup_installs i ON i.id=r.rollup_install_id
        WHERE i.has_authoritative=0 GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12
    ), missing AS (SELECT timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold, row_count, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, estimated_cost_microdollars, savings_microdollars, computed_request_count, computed_aggregate_count, reported_count, base_request_count FROM expected EXCEPT SELECT timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold, row_count, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, estimated_cost_microdollars, savings_microdollars, computed_request_count, computed_aggregate_count, reported_count, base_request_count FROM usage_day_totals),
    extra AS (SELECT timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold, row_count, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, estimated_cost_microdollars, savings_microdollars, computed_request_count, computed_aggregate_count, reported_count, base_request_count FROM usage_day_totals EXCEPT SELECT timezone_id, local_date, project, agent, machine, git_branch, is_automated, user_message_count, reported_model, provider_id, rate_hash, band_threshold, row_count, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, estimated_cost_microdollars, savings_microdollars, computed_request_count, computed_aggregate_count, reported_count, base_request_count FROM expected)
    SELECT (SELECT COUNT(*) FROM missing)+(SELECT COUNT(*) FROM extra)+(SELECT COUNT(*) FROM usage_day_totals WHERE row_count<=0)`
	var differences int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), query).Scan(&differences))
	assert.Zero(t, differences)
}

func TestUsageDayTotalsRefreshStaysJournaled(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC"}
	warmJournaledUsage(t, database, filter)
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "session", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:31:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":9}`)}}))
	var reads []bool
	database.usageCache.usageWindowObserver.afterRead = func(journaled bool) {
		reads = append(reads, journaled)
	}
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 109, got.Totals.InputTokens)
	assert.Equal(t, []bool{true}, reads, "the first read after an append must use the journaled path")
}

// CheckUsageDayTotalsFiltersMatchLegacy checks private cache state for the external fixture test.
func CheckUsageDayTotalsFiltersMatchLegacy(t *testing.T, database *DB) {
	t.Helper()
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
	prepared, err := database.prepareUsageWindow(t.Context(), filter, usageQueryKindDayTotals, true, true, nil)
	require.NoError(t, err)
	require.True(t, prepared.journaled)
	assert.Empty(t, prepared.snapshot.Sessions, "a warm totals read must skip archive window metadata")
	assert.Empty(t, prepared.snapshot.Versions)
	for _, tc := range []struct {
		filter UsageFilter
		owners []string
	}{
		{UsageFilter{From: "2026-08-05", To: "2026-08-05", Timezone: "UTC"}, nil},
		{UsageFilter{From: "2026-08-03", To: "2026-08-03", Timezone: "UTC", Agent: "codex"}, []string{"duplicate-a", "duplicate-b"}},
	} {
		prepared, err := database.prepareUsageWindow(t.Context(), tc.filter, usageQueryKindDayTotals, true, true, nil)
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

func TestUsageDayTotalsInstallDelete(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{Timezone: "UTC", Breakdowns: true}
	cache := warmJournaledUsage(t, database, filter)
	assertUsageDayTotals(t, cache)
	_, err := cache.db.ExecContext(t.Context(), `DELETE FROM usage_rollup_installs WHERE session_id='session'`)
	require.NoError(t, err)
	assertUsageDayTotals(t, cache)
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	want, err := database.getDailyUsageLegacy(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, mustUsageJSON(t, want), mustUsageJSON(t, got))
	assertUsageDayTotals(t, cache)
}

func TestUsageDayTotalsDeleteFirstPricingTimestamp(t *testing.T) {
	database := testDB(t)
	// Equal rates across adjacent periods share a hash. Both UTC dates fall
	// on one New York day, so deleting the first leaves its pricing timestamp.
	data := []byte(`[{"id":"test","name":"Test","api_pattern":"https://example.invalid","model_match":{"starts_with":"dated-"},
			"models":[{"id":"dated-model","match":{"equals":"dated-model"},"prices":[
				{"prices":{"input_mtok":1}},
				{"constraint":{"start_date":"2026-08-11"},"prices":{"input_mtok":1}},
				{"constraint":{"start_date":"2026-08-12"},"prices":{"input_mtok":2}}
			]}]}]`)
	prices, err := pricingpkg.ParseGenAIPrices(data)
	require.NoError(t, err)
	require.NoError(t, database.UpsertGenAIPricing(t.Context(), GenAIPricingDocument{
		Version: prices.Version(), Source: GenAIPricingSourceFetched, Data: data,
	}))
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "America/New_York", Breakdowns: true}
	insertSession(t, database, "first", "project-a", func(s *Session) { s.Agent = "codex"; s.StartedAt = new("2026-08-10T23:30:00Z") })
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "first", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T23:30:00Z", Model: "dated-model", TokenUsage: []byte(`{"input_tokens":100}`)}}))
	cache := warmJournaledUsage(t, database, filter)
	insertSession(t, database, "second", "project-a", func(s *Session) { s.Agent = "codex"; s.StartedAt = new("2026-08-11T01:30:00Z") })
	require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "second", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-11T01:30:00Z", Model: "dated-model", TokenUsage: []byte(`{"input_tokens":200}`)}}))
	_, err = database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	var hashes, timestamps int
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT COUNT(DISTINCT rate_hash), COUNT(DISTINCT pricing_timestamp) FROM usage_daily_rollups`).Scan(&hashes, &timestamps))
	require.Equal(t, 1, hashes, "both effective periods must contribute to the same totals key")
	require.Equal(t, 2, timestamps)
	var firstTimestamp string
	require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT pricing_timestamp FROM usage_day_totals`).Scan(&firstTimestamp))
	require.Equal(t, "2026-08-10T23:30:00Z", firstTimestamp)

	require.NoError(t, database.DeleteSession(t.Context(), "first"))
	got, err := database.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	want, err := database.getDailyUsageLegacy(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, 200, got.Totals.InputTokens)
	assert.Equal(t, int64(200), got.Totals.TotalCost.Microdollars)
	assert.Equal(t, mustUsageJSON(t, want), mustUsageJSON(t, got))
	assertUsageDayTotals(t, cache)
}

func TestUsageDayTotalsHistoricalRateCollisionMatchesLegacy(t *testing.T) {
	for _, sameSession := range []bool{false, true} {
		name := "separate sessions"
		if sameSession {
			name = "same session"
		}
		t.Run(name, func(t *testing.T) {
			database := testDB(t)
			// A computed $1 base rate bills at $1.10, equal to the next
			// period's reported base rate. Both periods fall on one local day.
			data := []byte(`[{"id":"test","name":"Test","api_pattern":"https://example.invalid","model_match":{"starts_with":"dated-"},
				"models":[{"id":"dated-model","match":{"equals":"dated-model"},"prices":[
					{"prices":{"input_mtok":1}},
					{"constraint":{"start_date":"2026-08-11"},"prices":{"input_mtok":1.1}}
				]}]}]`)
			prices, err := pricingpkg.ParseGenAIPrices(data)
			require.NoError(t, err)
			require.NoError(t, database.UpsertGenAIPricing(t.Context(), GenAIPricingDocument{Version: prices.Version(), Source: GenAIPricingSourceFetched, Data: data}))
			insertSession(t, database, "computed", "project-a", func(s *Session) { s.Agent = "posit-assistant"; s.StartedAt = new("2026-08-10T23:30:00Z") })
			require.NoError(t, database.InsertMessages(t.Context(), []Message{{SessionID: "computed", Ordinal: 0, Role: "assistant", Timestamp: "2026-08-10T23:30:00Z", Model: "dated-model", ProviderID: "positai", TokenUsage: []byte(`{"input_tokens":1000000}`)}}))
			reportedSession := "computed"
			if !sameSession {
				reportedSession = "reported"
				insertSession(t, database, reportedSession, "project-a", func(s *Session) { s.Agent = "posit-assistant"; s.StartedAt = new("2026-08-11T01:30:00Z") })
			}
			cost := money.MustParseDollars("2")
			require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(), reportedSession, []UsageEvent{{Source: "request", Model: "dated-model", ProviderID: "positai", InputTokens: 1_000_000, Cost: &cost, CostStatus: "exact", CostSource: "provider-reported", OccurredAt: "2026-08-11T01:30:00Z", DedupKey: "reported"}}))
			filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "America/New_York", Breakdowns: true}
			warmJournaledUsage(t, database, filter)
			got, err := database.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			want, err := database.getDailyUsageLegacy(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, 2_000_000, got.Totals.InputTokens)
			assert.Equal(t, money.MustParseDollars("3.1"), got.Totals.TotalCost)
			assert.Equal(t, mustUsageJSON(t, want), mustUsageJSON(t, got))
		})
	}
}

func mustUsageJSON(tb testing.TB, result DailyUsageResult) string {
	tb.Helper()
	raw, err := json.Marshal(result)
	require.NoError(tb, err)
	return string(raw)
}

func TestUsageDayTotalsBulkInstall(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{false: "fill", true: "pass"}[background], func(t *testing.T) {
			database := testDB(t)
			seedJournaledUsage(t, database, "first")
			seedJournaledUsage(t, database, "second")
			id, err := database.GetDatabaseID(t.Context())
			require.NoError(t, err)
			cache, err := database.usageCache.Generation(t.Context(), id)
			require.NoError(t, err)
			// Count persisted totals writes to catch a return to per-session upserts.
			_, err = cache.db.ExecContext(t.Context(), `CREATE TABLE totals_writes (n INTEGER NOT NULL);
				INSERT INTO totals_writes VALUES (0);
				CREATE TRIGGER count_totals_writes BEFORE INSERT ON usage_day_totals
				BEGIN UPDATE totals_writes SET n=n+1; END;`)
			require.NoError(t, err)
			filter := UsageFilter{Timezone: "UTC", Breakdowns: true}
			if background {
				require.NoError(t, database.StartUsageCacheBackfill(t.Context()))
				require.NoError(t, database.WaitUsageCacheBackfill(t.Context()))
				database.StopUsageCacheBackfill()
			} else {
				_, err = database.GetDailyUsage(t.Context(), filter)
				require.NoError(t, err)
			}
			var writes int
			require.NoError(t, cache.db.QueryRowContext(t.Context(), `SELECT n FROM totals_writes`).Scan(&writes))
			assert.Equal(t, 1, writes, "one totals upsert for the batch's shared day and dimensions")
			assertUsageDayTotals(t, cache)
			assertJournaledDailyUsageLegacy(t, database, filter)

			seedJournaledUsage(t, database, "third")
			got, err := database.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			want, err := database.getDailyUsageLegacy(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, 300, got.Totals.InputTokens)
			assert.Equal(t, mustUsageJSON(t, want), mustUsageJSON(t, got))
			assertUsageDayTotals(t, cache)

			// Replacing a batch subtracts the old rows before adding new totals.
			require.NoError(t, database.InsertMessages(t.Context(), []Message{
				{SessionID: "first", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:31:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":7}`)},
				{SessionID: "second", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-10T23:31:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":11}`)},
			}))
			got, err = database.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			want, err = database.getDailyUsageLegacy(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, 318, got.Totals.InputTokens)
			assert.Equal(t, mustUsageJSON(t, want), mustUsageJSON(t, got))
			assertUsageDayTotals(t, cache)
		})
	}
}
