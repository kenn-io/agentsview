package clickhouse

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func usagePriceTestRows(input string, updatedAt time.Time) []export.EffectivePricingRow {
	return []export.EffectivePricingRow{{
		ModelPattern: "tier-test",
		Rates: export.ModelRates{
			InputPerMTok:  money.MustParseDollars(input),
			OutputPerMTok: money.MustParseDollars("10"),
			UpdatedAt:     &updatedAt,
			Source:        export.PricingRowSourceFetched,
			Bands: []export.PricingBand{{
				AboveInputTokens: 1000,
				InputPerMTok:     money.MustParseDollars("2"),
				OutputPerMTok:    money.MustParseDollars("20"),
				UpdatedAt:        &updatedAt,
			}},
		},
	}}
}

func TestUsagePricingDigestIgnoresRowTimestamps(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base, err := chUsagePricingDigest(usagePriceTestRows("1", first), nil)
	require.NoError(t, err)
	republished, err := chUsagePricingDigest(
		usagePriceTestRows("1", first.Add(time.Hour)), nil)
	require.NoError(t, err)
	repriced, err := chUsagePricingDigest(usagePriceTestRows("1.5", first), nil)
	require.NoError(t, err)

	assert.Equal(t, base, republished)
	assert.NotEqual(t, base, repriced)
}

// A stored context must replay into the pricing block exactly what pricing
// every event would have recorded.
func TestUsagePriceContextReplaysPricingBlock(t *testing.T) {
	rows := usagePriceTestRows("1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	type event struct {
		input    chUsagePriceInput
		wantKind string
	}
	events := []event{
		{chUsagePriceInput{model: "tier-test", priceModel: "tier-test", source: "message", hasOrdinal: true, inputTok: 500, outputTok: 5}, chUsagePriceKindRequest},
		{chUsagePriceInput{model: "tier-test", priceModel: "tier-test", source: "message", hasOrdinal: true, inputTok: 900, cacheRd: 200}, chUsagePriceKindRequest},
		{chUsagePriceInput{model: "tier-test", priceModel: "tier-test", source: "message", hasOrdinal: true, inputTok: 4000}, chUsagePriceKindRequest},
		{chUsagePriceInput{model: "tier-test", priceModel: "tier-test", source: "hermes", inputTok: 5000}, chUsagePriceKindAggregate},
		{chUsagePriceInput{model: "tier-test", priceModel: "tier-test", source: "hermes", inputTok: 70, reported: true}, chUsagePriceKindReported},
		{chUsagePriceInput{model: "tier-test", priceModel: "tier-test", source: "message", hasOrdinal: true}, chUsagePriceKindZero},
		{chUsagePriceInput{model: "unknown-model", priceModel: "unknown-model", source: "message", hasOrdinal: true, inputTok: 9}, chUsagePriceKindRequest},
	}

	direct := export.NewPricingResolver(rows)
	replayed := export.NewPricingResolver(rows)
	pricer := export.NewPricingResolver(rows)
	for _, e := range events {
		in := e.input
		_, _, _, _, err := chUsageAggregateResolvedCost(
			in.model, in.priceModel, in.providerID, time.Time{},
			in.inputTok, in.outputTok, in.cacheCr, in.cacheCr1h, in.cacheRd,
			chBillable(in, in.inputTok), chBillable(in, in.outputTok), 0,
			chBillable(in, in.cacheCr), chBillable(in, in.cacheCr1h),
			chBillable(in, in.cacheRd), 0, 0, in.reported,
			in.hasOrdinal, direct)
		require.NoError(t, err)

		contexts := map[string]string{}
		rec, err := chPriceUsageInput(in, pricer, contexts)
		require.NoError(t, err)
		require.Empty(t, rec.priceError)
		contextID := rec.billedContextID
		if e.wantKind == chUsagePriceKindReported || e.wantKind == chUsagePriceKindZero {
			contextID = rec.unbilledContextID
		}
		stored, err := decodeUsagePriceContext(contexts[contextID])
		require.NoError(t, err)
		require.NoError(t, stored.record(replayed, e.wantKind, rec.bandAbove, 1))
	}

	want, err := direct.BuildBlock()
	require.NoError(t, err)
	got, err := replayed.BuildBlock()
	require.NoError(t, err)
	// Band timestamps never reach the wire and are not stored in a context.
	wantWire, err := json.Marshal(want.Models, json.Deterministic(true))
	require.NoError(t, err)
	gotWire, err := json.Marshal(got.Models, json.Deterministic(true))
	require.NoError(t, err)
	assert.JSONEq(t, string(wantWire), string(gotWire))
}

func chBillable(in chUsagePriceInput, tokens int) int {
	if in.reported {
		return 0
	}
	return tokens
}
