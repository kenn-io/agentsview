package readbase

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

// The usage query prices a row at the last schedule segment of its model
// and provider that starts at or before the row; a row without a timestamp
// prices at the segment from ActivityUsageNullPricingUS. Either way it must
// get the rate the resolver gives at the row's own instant, across
// time-of-day windows, catalog start dates, and the Kimi alias cutoff.
func TestUsageRateScheduleMatchesResolverAtEveryInstant(t *testing.T) {
	genAI, err := pricingpkg.ParseGenAIPrices([]byte(`[
		{
			"id": "offpeak",
			"name": "Off Peak",
			"api_pattern": "https://example.invalid",
			"model_match": {"contains": "night-model"},
			"models": [{
				"id": "night-model",
				"match": {"equals": "night-model"},
				"prices": [
					{"prices": {"input_mtok": 10}},
					{"constraint": {"start_date": "2026-07-20"}, "prices": {"input_mtok": 8}},
					{"constraint": {"start_time": "22:00:00+00:00", "end_time": "02:00:00+00:00"},
					 "prices": {"input_mtok": 2}}
				]
			}]
		}
	]`))
	require.NoError(t, err)
	pricing := export.NewPricingResolver([]export.EffectivePricingRow{
		{GenAI: genAI, GenAISource: export.PricingRowSourceEmbedded},
		{ModelPattern: pricingpkg.KimiK26Canonical, Rates: export.ModelRates{InputPerMTok: money.MustParseDollars("1")}},
		{ModelPattern: pricingpkg.KimiK3Canonical, Rates: export.ModelRates{InputPerMTok: money.MustParseDollars("4")}},
		{ModelPattern: "flat-model", Rates: export.ModelRates{InputPerMTok: money.MustParseDollars("3")}},
	})
	keys := []UsageRateKey{{Model: "flat-model"}, {Model: "kimi-for-coding"}, {Model: "night-model"}}
	start := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	schedule := BuildUsageRateSchedule(pricing, keys, CeilMicros(start), CeilMicros(end))
	scheduled := func(key UsageRateKey, us int64) export.UsageRate {
		var found *UsageRate
		for i, rate := range schedule.Rates {
			if rate.Model == key.Model && rate.ProviderID == key.ProviderID && rate.FromUS <= us {
				found = &schedule.Rates[i]
			}
		}
		require.NotNil(t, found, "no segment for %v at %d", key, us)
		return found.UsageRate
	}
	for _, key := range keys {
		want := pricing.UsageRateAt(key.ProviderID, key.Model, time.Time{})
		require.True(t, want.Equal(scheduled(key, ActivityUsageNullPricingUS)), "%s without a timestamp", key.Model)
	}
	rng := rand.New(rand.NewPCG(7, 11))
	span := end.Sub(start).Microseconds()
	instants := []time.Time{
		start, end.Add(-time.Microsecond), pricingpkg.KimiModelEraCutoff,
		pricingpkg.KimiModelEraCutoff.Add(-time.Microsecond),
		time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC), time.Date(2026, 7, 20, 1, 59, 59, 999999000, time.UTC),
		time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC),
	}
	for range 2000 {
		instants = append(instants, start.Add(time.Duration(rng.Int64N(span))*time.Microsecond))
	}
	for _, at := range instants {
		for _, key := range keys {
			want := pricing.UsageRateAt(key.ProviderID, key.Model, at)
			require.True(t, want.Equal(scheduled(key, at.UnixMicro())), "%s at %s", key.Model, at)
		}
	}
}
