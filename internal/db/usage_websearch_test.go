// ABOUTME: Tests that billed Anthropic web searches are read off usage rows
// ABOUTME: and priced at the flat per-request fee in every SQLite rollup.
package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestParseUsageWebSearchRequests(t *testing.T) {
	tests := []struct {
		name      string
		tokenJSON string
		want      int
	}{
		{name: "empty blob", tokenJSON: "", want: 0},
		{
			name:      "no server tool use",
			tokenJSON: `{"input_tokens":10,"output_tokens":5}`,
			want:      0,
		},
		{
			name: "zero counter",
			tokenJSON: `{"input_tokens":10,"server_tool_use":` +
				`{"web_search_requests":0,"web_fetch_requests":2}}`,
			want: 0,
		},
		{
			name: "nonzero counter",
			tokenJSON: `{"input_tokens":10,"server_tool_use":` +
				`{"web_search_requests":3,"web_fetch_requests":2}}`,
			want: 3,
		},
		{
			name: "counter after other nested keys",
			tokenJSON: `{"server_tool_use":{"web_fetch_requests":2,` +
				`"web_search_requests":7},"output_tokens":5}`,
			want: 7,
		},
		{
			name: "string counter",
			tokenJSON: `{"server_tool_use":` +
				`{"web_search_requests":"4"}}`,
			want: 4,
		},
		{
			name: "negative counter reads as zero",
			tokenJSON: `{"server_tool_use":` +
				`{"web_search_requests":-4}}`,
			want: 0,
		},
		{
			name: "web fetch alone is not a search",
			tokenJSON: `{"server_tool_use":` +
				`{"web_fetch_requests":9}}`,
			want: 0,
		},
		{
			name: "a same-named key elsewhere is not counted",
			tokenJSON: `{"metadata":{"server_tool_use":` +
				`{"web_search_requests":9}},"output_tokens":5}`,
			want: 0,
		},
		{
			name:      "truncated blob",
			tokenJSON: `{"input_tokens":10,"server_tool_use":{"web_sea`,
			want:      0,
		},
		{
			name:      "non-object server_tool_use",
			tokenJSON: `{"server_tool_use":5}`,
			want:      0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				parseUsageWebSearchRequests(tt.tokenJSON))
		})
	}
}

func TestUsageRowWebSearchRequestsIgnoresUsageEvents(t *testing.T) {
	const blob = `{"server_tool_use":{"web_search_requests":2}}`
	assert.Equal(t, 2, usageRowWebSearchRequests("message", blob))
	assert.Zero(t, usageRowWebSearchRequests("session", blob))
	assert.Zero(t, usageRowWebSearchRequests("usage_event", blob))
}

// webSearchUsageDB seeds one Claude session whose two assistant messages
// bill identical tokens, one of them alongside the given number of
// web searches.
// Pricing is $1/MTok in and $2/MTok out, so each message's token cost is
// exactly $0.30 (100_000 in, 100_000 out).
func webSearchUsageDB(t *testing.T, model string, searches int) *DB {
	t.Helper()
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-websearch-test",
		InputPerMTok:         money.MustParseDollars("1.0"),
		OutputPerMTok:        money.MustParseDollars("2.0"),
		CacheCreationPerMTok: money.MustParseDollars("0"),
		CacheReadPerMTok:     money.MustParseDollars("0"),
	}}), "UpsertModelPricing")

	insertSession(t, d, "sess-ws", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-07-30T10:00:00Z")
	})
	usage := func(webSearchRequests int) jsontext.Value {
		blob := `{"input_tokens":100000,"output_tokens":100000,` +
			`"cache_creation_input_tokens":0,` +
			`"cache_read_input_tokens":0,"server_tool_use":` +
			`{"web_search_requests":` +
			strconv.Itoa(webSearchRequests) +
			`,"web_fetch_requests":0}}`
		return jsontext.Value(blob)
	}
	insertMessages(t, d,
		Message{
			SessionID: "sess-ws", Ordinal: 0,
			Role:            "assistant",
			Timestamp:       "2026-07-30T10:01:00Z",
			Model:           model,
			TokenUsage:      usage(searches),
			OutputTokens:    100000,
			HasOutputTokens: true,
			ClaudeMessageID: "msg_ws_1",
			ClaudeRequestID: "req_ws_1",
		},
		Message{
			SessionID: "sess-ws", Ordinal: 1,
			Role:            "assistant",
			Timestamp:       "2026-07-30T10:02:00Z",
			Model:           model,
			TokenUsage:      usage(0),
			OutputTokens:    100000,
			HasOutputTokens: true,
			ClaudeMessageID: "msg_ws_2",
			ClaudeRequestID: "req_ws_2",
		},
	)
	return d
}

func TestSessionUsageBillsWebSearchRequests(t *testing.T) {
	ctx := context.Background()
	d := webSearchUsageDB(t, "claude-websearch-test", 2)

	usage, err := d.GetSessionUsage(ctx, "sess-ws", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.True(t, usage.HasCost)

	// Two messages at $0.30 of tokens each, plus two searches at $0.01.
	assert.Equal(t, money.MustParseDollars("0.62"), usage.Cost)
	require.Len(t, usage.Breakdown, 2)
	assert.Equal(t, 2, usage.Breakdown[0].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.32"),
		usage.Breakdown[0].Cost)
	assert.Zero(t, usage.Breakdown[1].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.30"),
		usage.Breakdown[1].Cost)
}

func TestSessionUsageBreakdownOmitsAbsentWebSearchRequests(t *testing.T) {
	ctx := context.Background()
	d := webSearchUsageDB(t, "claude-websearch-test", 0)

	usage, err := d.GetSessionUsage(ctx, "sess-ws", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Len(t, usage.Breakdown, 2)

	encoded, err := json.Marshal(usage.Breakdown[0])
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "web_search_requests")
	assert.Equal(t, money.MustParseDollars("0.60"), usage.Cost)
}

func TestDailyUsageBillsWebSearchRequestsOnce(t *testing.T) {
	ctx := context.Background()
	d := webSearchUsageDB(t, "claude-websearch-test", 2)

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-07-01",
		To:   "2026-07-31",
	})
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.62"),
		result.Totals.TotalCost)
}

// A duplicate of the same Claude message must not double the fee, the same
// way it must not double the tokens.
func TestDailyUsageWebSearchFeeSurvivesDedup(t *testing.T) {
	ctx := context.Background()
	d := webSearchUsageDB(t, "claude-websearch-test", 2)

	insertSession(t, d, "sess-ws-fork", "proj1", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-07-30T10:00:30Z")
		s.ParentSessionID = new("sess-ws")
		s.RelationshipType = "fork"
	})
	insertMessages(t, d, Message{
		SessionID: "sess-ws-fork", Ordinal: 0,
		Role:      "assistant",
		Timestamp: "2026-07-30T10:01:00Z",
		Model:     "claude-websearch-test",
		TokenUsage: jsontext.Value(
			`{"input_tokens":100000,"output_tokens":100000,` +
				`"server_tool_use":{"web_search_requests":2}}`),
		OutputTokens:    100000,
		HasOutputTokens: true,
		ClaudeMessageID: "msg_ws_1",
		ClaudeRequestID: "req_ws_1",
	})

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-07-01",
		To:   "2026-07-31",
	})
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.62"),
		result.Totals.TotalCost)
}

func TestAsymmetricClaudeSnapshotsPreserveWebSearchFee(t *testing.T) {
	ctx := context.Background()
	d := webSearchUsageDB(t, "claude-websearch-test", 2)
	insertMessages(t, d, Message{
		SessionID: "sess-ws", Ordinal: 2,
		Role:      "assistant",
		Timestamp: "2026-07-30T10:03:00Z",
		Model:     "claude-websearch-test",
		TokenUsage: jsontext.Value(
			`{"input_tokens":100000,"output_tokens":200000,` +
				`"server_tool_use":{"web_search_requests":0}}`),
		OutputTokens:    200000,
		HasOutputTokens: true,
		ClaudeMessageID: "msg_ws_1",
		ClaudeRequestID: "req_ws_1",
	})

	usage, err := d.GetSessionUsage(ctx, "sess-ws", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, money.MustParseDollars("0.82"), usage.Cost)
	require.Len(t, usage.Breakdown, 2)
	assert.Equal(t, 2, usage.Breakdown[1].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.52"),
		usage.Breakdown[1].Cost)

	daily, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-07-30", To: "2026-07-30", Timezone: "UTC",
	})
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.82"),
		daily.Totals.TotalCost)

	report, err := d.GetActivityReport(ctx,
		AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-07-30", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.82"), report.Totals.Cost)

	rowSet, err := d.GetSessionUsageRows(ctx, []string{"sess-ws"})
	require.NoError(t, err)
	require.Len(t, rowSet.Rows, 2)
	assert.Equal(t, 2, rowSet.Rows[1].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.52"), rowSet.Rows[1].Cost)
}

// An unpriced model still owes the flat fee: it is a known amount of real
// spend that does not depend on token rates. The row stays unpriced so the
// session is still reported as an incomplete estimate.
func TestSessionUsageBillsWebSearchOnUnpricedModel(t *testing.T) {
	ctx := context.Background()
	d := webSearchUsageDB(t, "some-unlisted-model", 2)

	usage, err := d.GetSessionUsage(ctx, "sess-ws", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.False(t, usage.HasCost)
	assert.Equal(t, []string{"some-unlisted-model"}, usage.UnpricedModels)
	require.Len(t, usage.Breakdown, 2)
	assert.Equal(t, money.MustParseDollars("0.02"),
		usage.Breakdown[0].Cost)
	assert.False(t, usage.Breakdown[0].HasCost)
	assert.Equal(t, 2, usage.Breakdown[0].WebSearchRequests)
	assert.Equal(t, money.Money{}, usage.Breakdown[1].Cost)

	result, err := d.GetDailyUsage(ctx, UsageFilter{
		From: "2026-07-01",
		To:   "2026-07-31",
	})
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.02"),
		result.Totals.TotalCost)
}

func TestActivityReportBillsWebSearchRequests(t *testing.T) {
	ctx := context.Background()
	d := webSearchUsageDB(t, "claude-websearch-test", 2)

	report, err := d.GetActivityReport(ctx,
		AnalyticsFilter{Timezone: "UTC"},
		dayQuery(t, "2026-07-30", "UTC"))
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.62"),
		report.Totals.Cost)
}

func TestSessionUsageRowsCarryWebSearchRequests(t *testing.T) {
	ctx := context.Background()
	d := webSearchUsageDB(t, "claude-websearch-test", 2)

	rowSet, err := d.GetSessionUsageRows(ctx, []string{"sess-ws"})
	require.NoError(t, err)
	rows := rowSet.Rows
	require.Len(t, rows, 2)
	assert.Equal(t, 2, rows[0].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.32"), rows[0].Cost)
	assert.Zero(t, rows[1].WebSearchRequests)
	assert.Equal(t, money.MustParseDollars("0.30"), rows[1].Cost)
}

// A row that carries its own reported cost is authoritative for the whole
// row, so the fee is not stacked on top of it.
func TestSessionRowCostSkipsWebSearchFeeOnReportedCost(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "claude-websearch-test",
		Rates: export.ModelRates{
			InputPerMTok:  money.MustParseDollars("1.0"),
			OutputPerMTok: money.MustParseDollars("2.0"),
		},
	}})
	row := usageScanRow{
		usageSource: "message",
		model:       "claude-websearch-test",
		tokenJSON: `{"input_tokens":100000,"output_tokens":100000,` +
			`"server_tool_use":{"web_search_requests":2}}`,
		cost: sql.NullInt64{Int64: 500_000, Valid: true},
	}
	cost, priced, contributes, err := sessionRowCost(row, resolver)
	require.NoError(t, err)
	assert.True(t, priced)
	assert.True(t, contributes)
	assert.Equal(t, money.MustParseDollars("0.50"), cost)
}
