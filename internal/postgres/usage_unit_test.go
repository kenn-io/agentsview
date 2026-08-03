package postgres

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func TestPGUsageDedupTokenForRowFallsBackToSourceUUIDWhenClaudePairIncomplete(t *testing.T) {
	got, ok := pgUsageDedupTokenForRow(
		"message",
		"claude-code",
		"msg-dup",
		"",
		"source-dup",
		"",
	)
	require.True(t, ok, "expected source_uuid fallback key")
	assert.Equal(t, pgUsageDedupToken{
		kind:  "source",
		value: "claude-code:source-dup",
	}, got)
}

func TestPGUsageAmountsPreserveSessionSummaryUsageEventTokens(t *testing.T) {
	rawInput := db.MaxPlausibleTokens + 250_000
	rawOutput := db.MaxPlausibleTokens + 500_000
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "gpt-5.4",
		Rates: export.ModelRates{
			InputPerMTok: money.MustParseDollars("1.0"), OutputPerMTok: money.MustParseDollars("2.0"),
		},
	}})

	inTok, outTok, _, _, cost, _, priceErr := pgDailyUsageAmounts(
		pgDailyUsageScanRow{
			usageSource:  "session",
			model:        "gpt-5.4",
			inputTokens:  rawInput,
			outputTokens: rawOutput,
		},
		resolver,
	)
	require.NoError(t, priceErr)
	assert.Equal(t, rawInput, inTok, "daily input")
	assert.Equal(t, rawOutput, outTok, "daily output")
	wantCost, err := money.CostPerMillion([]money.RatedTokens{
		{Tokens: int64(rawInput), Rate: money.MustParseDollars("1")},
		{Tokens: int64(rawOutput), Rate: money.MustParseDollars("2")},
	})
	require.NoError(t, err)
	assert.Equal(t, wantCost, cost, "daily cost")

	cost, priced, contributes, priceErr := pgSessionRowCost(pgUsageScanRow{
		usageSource:  "session",
		model:        "gpt-5.4",
		inputTokens:  rawInput,
		outputTokens: rawOutput,
	}, resolver)
	require.NoError(t, priceErr)
	require.True(t, priced, "priced")
	require.True(t, contributes, "contributes")
	assert.Equal(t, wantCost, cost, "session cost")
}

func TestPGDailyUsageAmountsPricingBandRequestScope(t *testing.T) {
	tests := []struct {
		name           string
		usageSource    string
		messageOrdinal sql.NullInt64
		wantCost       int64
		wantAggregate  int
		wantBand       int
	}{
		{
			name:           "ordinal-bound request uses band",
			usageSource:    "usage-event",
			messageOrdinal: sql.NullInt64{Int64: 1, Valid: true},
			wantCost:       600_000,
			wantBand:       1,
		},
		{
			name:        "Goose request uses band without message ordinal",
			usageSource: "goose-request",
			wantCost:    600_000,
			wantBand:    1,
		},
		{
			name:          "unbound aggregate uses base",
			usageSource:   "usage-event",
			wantCost:      300_000,
			wantAggregate: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := pgPricingBandTestResolver()
			_, _, _, _, cost, _, err := pgDailyUsageAmounts(pgDailyUsageScanRow{
				messageOrdinal: tt.messageOrdinal,
				usageSource:    tt.usageSource,
				model:          "banded-model",
				inputTokens:    300_000,
			}, resolver)
			require.NoError(t, err)
			block, err := resolver.BuildBlock()
			require.NoError(t, err)
			provenance := block.Models["banded-model"]
			require.Len(t, provenance.Resolutions, 1)
			application := provenance.Resolutions[0].Application

			assert.Equal(t, money.Money{Microdollars: tt.wantCost}, cost)
			assert.Equal(t, tt.wantAggregate, application.AggregateRowCount)
			if tt.wantBand > 0 {
				require.Len(t, application.Bands, 1)
				assert.Equal(t, tt.wantBand, application.Bands[0].RequestCount)
			}
		})
	}
}

func pgPricingBandTestResolver() *export.PricingResolver {
	return export.NewPricingResolver([]export.EffectivePricingRow{{
		ModelPattern: "banded-model",
		Rates: export.ModelRates{
			InputPerMTok:      money.MustParseDollars("1"),
			OutputPerMTok:     money.MustParseDollars("2"),
			CacheWritePerMTok: money.MustParseDollars("0.50"),
			CacheReadPerMTok:  money.MustParseDollars("0.10"),
			Bands: []export.PricingBand{{
				AboveInputTokens:  200_000,
				InputPerMTok:      money.MustParseDollars("2"),
				OutputPerMTok:     money.MustParseDollars("3"),
				CacheWritePerMTok: money.MustParseDollars("1"),
				CacheReadPerMTok:  money.MustParseDollars("0.20"),
			}},
		},
	}})
}

func TestPGUsageRowQueryPushesDateBoundsIntoUnion(t *testing.T) {
	pb := &paramBuilder{}
	query := pgUsageRowQuery(pb, db.UsageFilter{
		From:             "2024-06-01",
		To:               "2024-06-30",
		ExcludeAutomated: true,
	})

	normalized := strings.ToLower(query)
	assert.NotContains(t, normalized, "and u.ts >=")
	assert.NotContains(t, normalized, "and u.ts <=")
	assert.NotContains(t, normalized, " or ")
	assert.NotContains(t, normalized, "display_name")
	assert.NotContains(t, normalized, "first_message")
	assert.NotContains(t, normalized, "cost_status")
	assert.Contains(t, normalized, "u.cost_source")
	assert.Contains(t, normalized, "u.reasoning_tokens")
	assert.NotContains(t, normalized, "user_message_count")
	assert.NotContains(t, normalized, "session_activity_at")
	assert.NotContains(t, normalized, " as started_at")
	assert.NotContains(t, normalized, "u.machine")
	assert.Contains(t, normalized, "message_timestamp_rows as materialized")
	assert.Contains(t, normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(t, normalized, "from message_timestamp_rows m\njoin sessions s")
	assert.Contains(t, normalized, "from usage_event_timestamp_rows ue\njoin sessions s")
	assert.Contains(t, normalized, "m.timestamp is not null")
	assert.Contains(t, normalized, "ue.occurred_at is not null")
	assert.Contains(t, normalized, "m.timestamp is null")
	assert.Contains(t, normalized, "ue.occurred_at is null")
	assert.Contains(t, normalized, "m.timestamp >= $1::timestamptz")
	assert.Contains(t, normalized, "ue.occurred_at >= $1::timestamptz")
	assert.Contains(t, normalized, "s.started_at >= $1::timestamptz")
	assert.Contains(t, normalized, "m.timestamp <= $2::timestamptz")
	assert.Contains(t, normalized, "ue.occurred_at <= $2::timestamptz")
	assert.Contains(t, normalized, "s.started_at <= $2::timestamptz")
	require.Len(t, pb.args, 2)
	assert.Equal(t, "2024-05-31T10:00:00Z", pb.args[0])
	assert.Equal(t, "2024-07-01T13:59:59Z", pb.args[1])
}

func TestPGDailyUsageDetailedQuerySelectsMachine(t *testing.T) {
	detailedParams := &paramBuilder{}
	detailed := strings.ToLower(pgDailyUsageRowQuery(
		detailedParams,
		db.UsageFilter{
			From: "2026-07-15", To: "2026-07-15", Breakdowns: true,
		},
		false,
	))
	assert.Contains(t, detailed, "u.machine")

	fastParams := &paramBuilder{}
	fast := strings.ToLower(pgDailyUsageRowQuery(
		fastParams,
		db.UsageFilter{From: "2026-07-15", To: "2026-07-15"},
		false,
	))
	assert.NotContains(t, fast, "u.machine")
}

func TestPGBoundedDailyUsageRowsCTEProjectsReasoningTokens(t *testing.T) {
	pb := &paramBuilder{}
	f := db.UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	}
	query := pgDailyUsageRowsSQLForBounds(pb, f, pgUsageBoundsForFilter(pb, f))

	normalized := strings.ToLower(query)
	assert.Contains(t, normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(t, normalized, "ue.cache_read_input_tokens,\n\t\tue.reasoning_tokens,\n\t\tue.cost_microdollars")
	assert.Contains(t, normalized, "from usage_event_timestamp_rows ue\njoin sessions s")
}

// TestPGMatchingUsageRowsSQLForBoundsRelaxesTokenEligibility asserts the
// bounded matching-session query relaxes token eligibility (no
// m.token_usage check) and model-presence eligibility (m.role = 'assistant'
// instead of m.model != ”, since some Copilot assistant messages parse
// before a model name is known) while filtering Model/ExcludeModel
// directly on the bounded message/event row, matching the normal bounded
// path instead of folding in a session-wide model-match EXISTS clause.
// Mirrors TestPGUsageRowQueryPushesDateBoundsIntoUnion's direct-call style
// with no live DB and no probe mock.
func TestPGMatchingUsageRowsSQLForBoundsRelaxesTokenEligibility(t *testing.T) {
	pb := &paramBuilder{}
	f := db.UsageFilter{
		From:  "2024-06-01",
		To:    "2024-06-30",
		Model: "gpt-5.3-codex",
	}
	bounds := pgUsageBoundsForFilter(pb, f)
	query := pgMatchingUsageRowsSQLForBounds(pb, f, bounds)

	normalized := strings.ToLower(query)
	assert.Contains(t, normalized, "message_timestamp_rows as materialized")
	assert.Contains(t, normalized, "usage_event_timestamp_rows as materialized")
	assert.Contains(t, normalized, "m.role = 'assistant'")
	assert.NotContains(t, normalized, "m.model != ''")
	assert.NotContains(t, normalized, "m.token_usage != ''")
	// Model is filtered on the bounded row directly, not via a
	// session-wide EXISTS: each of the four branches (message-timestamp
	// source, event-timestamp source, message fallback, event fallback)
	// applies its own m.model/ue.model comparison, no EXISTS subqueries.
	assert.Equal(t, 0, strings.Count(normalized, "exists ("))
	assert.Equal(t, 2, strings.Count(normalized, "m.model = "))
	assert.Equal(t, 2, strings.Count(normalized, "ue.model = "))
}

func TestPGTopSessionsUsageRowQueryUsesNarrowScan(t *testing.T) {
	pb := &paramBuilder{}
	query := pgTopSessionsUsageRowQuery(pb, db.UsageFilter{
		From: "2024-06-01",
		To:   "2024-06-30",
	})

	normalized := strings.ToLower(query)
	assert.NotContains(t, normalized, "display_name")
	assert.NotContains(t, normalized, "first_message")
	assert.NotContains(t, normalized, "cost_status")
	assert.Contains(t, normalized, "u.cost_source")
	assert.Contains(t, normalized, "u.reasoning_tokens")
	assert.NotContains(t, normalized, "user_message_count")
	assert.NotContains(t, normalized, "session_activity_at")
	assert.NotContains(t, normalized, " as started_at")
	assert.NotContains(t, normalized, "u.machine")
	assert.Contains(t, normalized, "m.timestamp is not null")
	assert.Contains(t, normalized, "ue.occurred_at is not null")
	assert.Contains(t, normalized, "m.timestamp is null")
	assert.Contains(t, normalized, "ue.occurred_at is null")
	assert.Contains(t, normalized, "m.timestamp >= $1::timestamptz")
	assert.Contains(t, normalized, "ue.occurred_at >= $1::timestamptz")
	assert.Contains(t, normalized,
		"m.timestamp is null\n\tand s.started_at >= $1::timestamptz")
	assert.Contains(t, normalized,
		"ue.occurred_at is null\n\tand s.started_at >= $1::timestamptz")
	assert.Contains(t, normalized, "m.timestamp <= $2::timestamptz")
	assert.Contains(t, normalized, "ue.occurred_at <= $2::timestamptz")
	require.Len(t, pb.args, 2)
	assert.Equal(t, "2024-05-31T10:00:00Z", pb.args[0])
	assert.Equal(t, "2024-07-01T13:59:59Z", pb.args[1])
}

func TestPGSessionRowCostIncludesReasoningOnlyRows(t *testing.T) {
	resolver := export.NewPricingResolver(
		[]export.EffectivePricingRow{{
			ModelPattern: "reasoning-model",
			Rates: export.ModelRates{
				OutputPerMTok: money.MustParseDollars("20"),
				Source:        export.PricingRowSourceFetched,
			},
		}},
	)

	cost, priced, contributes, err := pgSessionRowCost(pgUsageScanRow{
		usageSource:     "provider",
		model:           "reasoning-model",
		reasoningTokens: 25,
	}, resolver)

	require.NoError(t, err)
	assert.True(t, contributes)
	assert.True(t, priced)
	assert.Equal(t, money.MustParseDollars("0.0005"), cost)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	require.Contains(t, block.Models, "reasoning-model")
	assert.Equal(t, export.CostSourceComputed,
		block.Models["reasoning-model"].CostSource)
}

func TestPGActivityReportRowStatusCanonicalizesKimiAliasByTimestamp(t *testing.T) {
	tests := []struct {
		name         string
		timestamp    time.Time
		canonical    string
		expectedCost money.Money
	}{
		{
			name:         "before cutoff",
			timestamp:    pricingpkg.KimiModelEraCutoff.Add(-time.Second),
			canonical:    pricingpkg.KimiK26Canonical,
			expectedCost: money.MustParseDollars("1"),
		},
		{
			name:         "at cutoff",
			timestamp:    pricingpkg.KimiModelEraCutoff,
			canonical:    pricingpkg.KimiK3Canonical,
			expectedCost: money.MustParseDollars("2"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := export.NewPricingResolver([]export.EffectivePricingRow{
				{
					ModelPattern: pricingpkg.KimiK26Canonical,
					Rates: export.ModelRates{
						InputPerMTok: money.MustParseDollars("1"),
					},
				},
				{
					ModelPattern: pricingpkg.KimiK3Canonical,
					Rates: export.ModelRates{
						InputPerMTok: money.MustParseDollars("2"),
					},
				},
			})

			cost, priced, contributes, err := pgActivityReportRowStatus(
				pgDailyUsageScanRow{
					usageSource: "provider",
					model:       "daimon-kimi-code",
					ts:          sql.NullTime{Time: tt.timestamp, Valid: true},
					inputTokens: 1_000_000,
				},
				resolver,
			)

			require.NoError(t, err)
			assert.True(t, priced)
			assert.True(t, contributes)
			assert.Equal(t, tt.expectedCost, cost)
			block, err := resolver.BuildBlock()
			require.NoError(t, err)
			require.Contains(t, block.Models, "daimon-kimi-code")
			resolutions := block.Models["daimon-kimi-code"].Resolutions
			require.Len(t, resolutions, 1)
			assert.Equal(t, tt.canonical, resolutions[0].PricedModel)
			assert.NotContains(t, block.Models, tt.canonical)
		})
	}
}

func TestPGActivityReportRowStatusPrefersExactCustomKimiAlias(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{
		{
			ModelPattern: "daimon-kimi-code",
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("7"),
				Source:       export.PricingRowSourceCustom,
			},
		},
		{
			ModelPattern: pricingpkg.KimiK3Canonical,
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("2"),
				Source:       export.PricingRowSourceFetched,
			},
		},
	})

	cost, priced, contributes, err := pgActivityReportRowStatus(
		pgDailyUsageScanRow{
			usageSource: "provider",
			model:       "daimon-kimi-code",
			ts: sql.NullTime{
				Time:  pricingpkg.KimiModelEraCutoff,
				Valid: true,
			},
			inputTokens: 1_000_000,
		},
		resolver,
	)

	require.NoError(t, err)
	assert.True(t, priced)
	assert.True(t, contributes)
	assert.Equal(t, money.MustParseDollars("7"), cost)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	require.Contains(t, block.Models, "daimon-kimi-code")
	resolutions := block.Models["daimon-kimi-code"].Resolutions
	require.Len(t, resolutions, 1)
	assert.Equal(t, "daimon-kimi-code", resolutions[0].PricedModel)
}

func TestPGUsageAmountsIncludeMessageReasoningTokens(t *testing.T) {
	resolver := export.NewPricingResolver(
		[]export.EffectivePricingRow{{
			ModelPattern: "gpt-5.4",
			Rates: export.ModelRates{
				InputPerMTok:  money.MustParseDollars("1"),
				OutputPerMTok: money.MustParseDollars("2"),
			},
		}},
	)
	row := pgDailyUsageScanRow{
		usageSource: "message",
		model:       "gpt-5.4",
		tokenJSON: `{"input_tokens":1000,"output_tokens":0,` +
			`"reasoning_tokens":500}`,
	}

	inTok, outTok, _, _, cost, _, err := pgDailyUsageAmounts(row, resolver)
	require.NoError(t, err)
	assert.Equal(t, 1000, inTok)
	assert.Zero(t, outTok)
	assert.Equal(t, money.MustParseDollars("0.002"), cost)

	sessionCost, priced, contributes, err := pgSessionRowCost(pgUsageScanRow{
		usageSource: "message",
		model:       "gpt-5.4",
		tokenJSON:   row.tokenJSON,
	}, resolver)
	require.NoError(t, err)
	assert.True(t, priced)
	assert.True(t, contributes)
	assert.Equal(t, money.MustParseDollars("0.002"), sessionCost)
}

func TestPGDailyUsageAmountsPrefersExactCustomKimiAlias(t *testing.T) {
	resolver := export.NewPricingResolver([]export.EffectivePricingRow{
		{
			ModelPattern: "kimi-for-coding",
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("7"),
				Source:       export.PricingRowSourceCustom,
			},
		},
		{
			ModelPattern: pricingpkg.KimiK3Canonical,
			Rates: export.ModelRates{
				InputPerMTok: money.MustParseDollars("2"),
				Source:       export.PricingRowSourceFetched,
			},
		},
	})

	_, _, _, _, cost, _, err := pgDailyUsageAmounts(pgDailyUsageScanRow{
		usageSource: "provider",
		model:       "kimi-for-coding",
		ts: sql.NullTime{
			Time:  pricingpkg.KimiModelEraCutoff,
			Valid: true,
		},
		inputTokens: 1_000_000,
	}, resolver)

	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("7"), cost)
	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	require.Contains(t, block.Models, "kimi-for-coding")
	resolutions := block.Models["kimi-for-coding"].Resolutions
	require.Len(t, resolutions, 1)
	assert.Equal(t, "kimi-for-coding", resolutions[0].PricedModel)
}
