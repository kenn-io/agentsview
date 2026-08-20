//go:build !(windows && arm64)

// ABOUTME: Store-contract parity for the Anthropic web search fee: the
// ABOUTME: DuckDB mirror must charge it exactly like the SQLite archive.
package duckdb

import (
	"context"
	"encoding/json/jsontext"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// webSearchPricing prices the fixture model at $1/MTok in and $2/MTok out,
// so each fixture message's token cost is exactly $0.30.
var webSearchPricing = []db.ModelPricing{{
	ModelPattern:  "claude-websearch-test",
	InputPerMTok:  money.MustParseDollars("1.0"),
	OutputPerMTok: money.MustParseDollars("2.0"),
}}

// webSearchWrites seeds one Claude session with two identical assistant
// turns, the first of which was billed for `searches` web searches.
func webSearchWrites(model string, searches int) []db.SessionBatchWrite {
	usage := func(requests int) jsontext.Value {
		return jsontext.Value(
			`{"input_tokens":100000,"output_tokens":100000,` +
				`"server_tool_use":{"web_search_requests":` +
				strconv.Itoa(requests) + `,"web_fetch_requests":0}}`)
	}
	message := func(ordinal int, clock string, requests int) db.Message {
		return db.Message{
			SessionID: "duck-ws", Ordinal: ordinal,
			Role:            "assistant",
			Content:         "work",
			Timestamp:       "2026-07-30T" + clock + "Z",
			Model:           model,
			TokenUsage:      usage(requests),
			OutputTokens:    100000,
			HasOutputTokens: true,
			ClaudeMessageID: "m-" + strconv.Itoa(ordinal),
			ClaudeRequestID: "r-" + strconv.Itoa(ordinal),
		}
	}
	return []db.SessionBatchWrite{{
		Session: db.Session{
			ID: "duck-ws", Project: "duck-websearch", Machine: "local",
			Agent:                "claude",
			StartedAt:            new("2026-07-30T10:00:00Z"),
			EndedAt:              new("2026-07-30T10:05:00Z"),
			MessageCount:         2,
			TotalOutputTokens:    200000,
			HasTotalOutputTokens: true,
		},
		Messages: []db.Message{
			message(0, "10:01:00", searches),
			message(1, "10:02:00", 0),
		},
		DataVersion: 1, ReplaceMessages: true,
	}}
}

// webSearchStores seeds the fixture into a local SQLite DB, pushes it into
// a DuckDB mirror, and returns both so every assertion can compare them.
func webSearchStores(
	t *testing.T, model string, searches int,
) (*db.DB, *Store) {
	return webSearchStoresFromWrites(t, webSearchWrites(model, searches))
}

func webSearchStoresFromWrites(
	t *testing.T, writes []db.SessionBatchWrite,
) (*db.DB, *Store) {
	t.Helper()
	ctx := context.Background()
	local := newLocalDB(t)
	require.NoError(t, local.UpsertModelPricing(webSearchPricing))
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	return local, NewStoreFromDB(syncer.DB())
}

func asymmetricWebSearchWrites() []db.SessionBatchWrite {
	writes := webSearchWrites("claude-websearch-test", 2)
	writes[0].Session.MessageCount = 3
	writes[0].Session.TotalOutputTokens = 400000
	writes[0].Messages = append(writes[0].Messages, db.Message{
		SessionID: "duck-ws", Ordinal: 2,
		Role:      "assistant",
		Content:   "fuller snapshot",
		Timestamp: "2026-07-30T10:03:00Z",
		Model:     "claude-websearch-test",
		TokenUsage: jsontext.Value(
			`{"input_tokens":100000,"output_tokens":200000,` +
				`"server_tool_use":{"web_search_requests":0}}`),
		OutputTokens:    200000,
		HasOutputTokens: true,
		ClaudeMessageID: "m-0",
		ClaudeRequestID: "r-0",
	})
	return writes
}

func TestDuckSessionUsageBillsWebSearchRequestsLikeSQLite(t *testing.T) {
	ctx := context.Background()
	local, duck := webSearchStores(t, "claude-websearch-test", 2)

	sqliteGot, err := local.GetSessionUsage(ctx, "duck-ws", true)
	require.NoError(t, err)
	require.NotNil(t, sqliteGot)
	duckGot, err := duck.GetSessionUsage(ctx, "duck-ws", true)
	require.NoError(t, err)
	require.NotNil(t, duckGot)

	// Two turns at $0.30 of tokens each, plus two searches at $0.01.
	assert.Equal(t, money.MustParseDollars("0.62"), sqliteGot.Cost)
	assert.Equal(t, sqliteGot.Cost, duckGot.Cost)
	assert.Equal(t, sqliteGot.HasCost, duckGot.HasCost)
	require.Len(t, duckGot.Breakdown, 2)
	assert.Equal(t, sqliteGot.Breakdown, duckGot.Breakdown)
	assert.Equal(t, 2, duckGot.Breakdown[0].WebSearchRequests)
	assert.Zero(t, duckGot.Breakdown[1].WebSearchRequests)
}

func TestDuckDailyUsageBillsWebSearchRequestsLikeSQLite(t *testing.T) {
	ctx := context.Background()
	local, duck := webSearchStores(t, "claude-websearch-test", 2)
	filter := db.UsageFilter{From: "2026-07-01", To: "2026-07-31"}

	sqliteGot, err := local.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	duckGot, err := duck.GetDailyUsage(ctx, filter)
	require.NoError(t, err)

	assert.Equal(t, money.MustParseDollars("0.62"),
		sqliteGot.Totals.TotalCost)
	assert.Equal(t, sqliteGot.Totals.TotalCost, duckGot.Totals.TotalCost)
}

func TestDuckWebSearchOnlyChargeMakesReportedSessionMixed(t *testing.T) {
	reportedCost := money.MustParseDollars("0.03")
	writes := []db.SessionBatchWrite{{
		Session: db.Session{
			ID: "duck-ws-mixed", Project: "duck-websearch", Machine: "local",
			Agent: "claude", StartedAt: new("2026-07-30T10:00:00Z"),
			EndedAt: new("2026-07-30T10:05:00Z"), MessageCount: 1,
		},
		Messages: []db.Message{{
			SessionID: "duck-ws-mixed", Ordinal: 0, Role: "assistant",
			Content: "searched", Timestamp: "2026-07-30T10:01:00Z",
			Model: "claude-websearch-test",
			TokenUsage: jsontext.Value(
				`{"input_tokens":0,"output_tokens":0,` +
					`"server_tool_use":{"web_search_requests":2}}`),
			ClaudeMessageID: "m-web", ClaudeRequestID: "r-web",
		}},
		UsageEvents: []db.UsageEvent{{
			SessionID: "duck-ws-mixed", Source: "usage",
			Model: "claude-websearch-test", Cost: &reportedCost,
			CostStatus: "exact", CostSource: "reported",
			OccurredAt: "2026-07-30T10:02:00Z", DedupKey: "reported-cost",
		}},
		DataVersion: 1, ReplaceMessages: true,
	}}
	ctx := context.Background()
	local, duck := webSearchStoresFromWrites(t, writes)

	sqliteSession, err := local.GetSessionUsage(ctx, "duck-ws-mixed", false)
	require.NoError(t, err)
	require.NotNil(t, sqliteSession)
	duckSession, err := duck.GetSessionUsage(ctx, "duck-ws-mixed", false)
	require.NoError(t, err)
	require.NotNil(t, duckSession)
	assert.Equal(t, money.MustParseDollars("0.05"), sqliteSession.Cost)
	assert.Equal(t, sqliteSession.Cost, duckSession.Cost)
	assert.Equal(t, export.CostSourceMixed, sqliteSession.CostSource)
	assert.Equal(t, sqliteSession.CostSource, duckSession.CostSource)

	filter := db.UsageFilter{From: "2026-07-30", To: "2026-07-30"}
	sqliteDaily, err := local.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	duckDaily, err := duck.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.NotNil(t, sqliteDaily.Pricing)
	require.NotNil(t, duckDaily.Pricing)
	assert.Equal(t, export.CostSourceMixed, sqliteDaily.Pricing.CostSource)
	assert.Equal(t, sqliteDaily.Pricing.CostSource,
		duckDaily.Pricing.CostSource)
}

func TestDuckAsymmetricClaudeSnapshotsPreserveWebSearchFeeLikeSQLite(
	t *testing.T,
) {
	ctx := context.Background()
	local, duck := webSearchStoresFromWrites(t, asymmetricWebSearchWrites())

	sqliteSession, err := local.GetSessionUsage(ctx, "duck-ws", true)
	require.NoError(t, err)
	require.NotNil(t, sqliteSession)
	duckSession, err := duck.GetSessionUsage(ctx, "duck-ws", true)
	require.NoError(t, err)
	require.NotNil(t, duckSession)
	assert.Equal(t, money.MustParseDollars("0.82"), sqliteSession.Cost)
	assert.Equal(t, sqliteSession.Cost, duckSession.Cost)

	filter := db.UsageFilter{From: "2026-07-30", To: "2026-07-30"}
	sqliteDaily, err := local.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	duckDaily, err := duck.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.82"),
		sqliteDaily.Totals.TotalCost)
	assert.Equal(t, sqliteDaily.Totals.TotalCost,
		duckDaily.Totals.TotalCost)

	sqliteReport, err := local.GetActivityReport(ctx,
		db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-07-30", "UTC"))
	require.NoError(t, err)
	duckReport, err := duck.GetActivityReport(ctx,
		db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-07-30", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.82"),
		sqliteReport.Totals.Cost)
	assert.Equal(t, sqliteReport.Totals.Cost, duckReport.Totals.Cost)
}

func TestDuckActivityReportBillsWebSearchRequestsLikeSQLite(t *testing.T) {
	ctx := context.Background()
	local, duck := webSearchStores(t, "claude-websearch-test", 2)

	sqliteGot, err := local.GetActivityReport(ctx,
		db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-07-30", "UTC"))
	require.NoError(t, err)
	duckGot, err := duck.GetActivityReport(ctx,
		db.AnalyticsFilter{Timezone: "UTC"},
		duckDayQuery(t, "2026-07-30", "UTC"))
	require.NoError(t, err)

	assert.Equal(t, money.MustParseDollars("0.62"), sqliteGot.Totals.Cost)
	assert.Equal(t, sqliteGot.Totals.Cost, duckGot.Totals.Cost)
}

func TestDuckSessionUsageRowsCarryWebSearchRequestsLikeSQLite(t *testing.T) {
	ctx := context.Background()
	local, duck := webSearchStores(t, "claude-websearch-test", 2)

	sqliteRowSet, err := local.GetSessionUsageRows(ctx, []string{"duck-ws"})
	require.NoError(t, err)
	duckRowSet, err := duck.GetSessionUsageRows(ctx, []string{"duck-ws"})
	require.NoError(t, err)
	sqliteRows := sqliteRowSet.Rows
	duckRows := duckRowSet.Rows

	require.Len(t, duckRows, 2)
	assert.Equal(t, sqliteRows, duckRows)
	assert.Equal(t, 2, duckRows[0].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.32"), duckRows[0].Cost)
	assert.Zero(t, duckRows[1].WebSearchRequests)
}

func TestDuckWebSearchFeeOnUnpricedModelMatchesSQLite(t *testing.T) {
	ctx := context.Background()
	local, duck := webSearchStores(t, "some-unlisted-model", 2)

	sqliteGot, err := local.GetSessionUsage(ctx, "duck-ws", true)
	require.NoError(t, err)
	require.NotNil(t, sqliteGot)
	duckGot, err := duck.GetSessionUsage(ctx, "duck-ws", true)
	require.NoError(t, err)
	require.NotNil(t, duckGot)

	assert.False(t, sqliteGot.HasCost)
	assert.Equal(t, sqliteGot.HasCost, duckGot.HasCost)
	assert.Equal(t, sqliteGot.UnpricedModels, duckGot.UnpricedModels)
	require.Len(t, duckGot.Breakdown, 2)
	assert.Equal(t, money.MustParseDollars("0.02"),
		duckGot.Breakdown[0].Cost)
	assert.Equal(t, sqliteGot.Breakdown, duckGot.Breakdown)

	filter := db.UsageFilter{From: "2026-07-01", To: "2026-07-31"}
	sqliteDaily, err := local.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	duckDaily, err := duck.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.02"),
		sqliteDaily.Totals.TotalCost)
	assert.Equal(t, sqliteDaily.Totals.TotalCost,
		duckDaily.Totals.TotalCost)
}
