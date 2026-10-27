package export

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/money"

	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func TestResolveBilledScalesEveryRateFieldAndBand(t *testing.T) {
	r := ModelRates{
		InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000},
		CacheWritePerMTok: money.Money{Microdollars: 3_000_000}, CacheWrite1hPerMTok: money.Money{Microdollars: 5_000_000}, CacheReadPerMTok: money.Money{Microdollars: 4_000_000},
		Bands: []PricingBand{
			{AboveInputTokens: 1, InputPerMTok: money.Money{Microdollars: 5}, OutputPerMTok: money.Money{Microdollars: 15}, CacheWritePerMTok: money.Money{Microdollars: 25}, CacheWrite1hPerMTok: money.Money{Microdollars: 30}, CacheReadPerMTok: money.Money{Microdollars: 35}},
			{AboveInputTokens: 2, InputPerMTok: money.Money{Microdollars: 45}, OutputPerMTok: money.Money{Microdollars: 55}, CacheWritePerMTok: money.Money{Microdollars: 65}, CacheWrite1hPerMTok: money.Money{Microdollars: 70}, CacheReadPerMTok: money.Money{Microdollars: 75}},
		},
	}
	resolver := NewPricingResolver([]EffectivePricingRow{{ModelPattern: "m", Rates: r}})
	_, got, err := resolver.ResolveBilled(pricingpkg.PositAssistantProviderID, "m", "m")
	if err != nil {
		require.FailNow(t, fmt.Sprint(err))
	}
	want := func(v money.Money) money.Money {
		out, err := money.ScaleRatio(v, 11, 10)
		if err != nil {
			require.FailNow(t, fmt.Sprint(err))
		}
		return out
	}
	assertScaledMoneyFields(t, reflect.ValueOf(r), reflect.ValueOf(got.Rates), "ModelRates", want)
	t.Log("scaled every ModelRates and PricingBand money field by 11/10=1.1")
}

func assertScaledMoneyFields(t *testing.T, before, after reflect.Value, scope string, want func(money.Money) money.Money) {
	t.Helper()

	if before.Type() != after.Type() {
		require.FailNowf(t, "test failed", "%s type changed: %v -> %v", scope, before.Type(), after.Type())
	}
	moneyType := reflect.TypeFor[money.Money]()
	seen := 0
	for i := range before.NumField() {
		field := before.Type().Field(i)
		if field.Type == moneyType {
			seen++
			got := after.Field(i).Interface().(money.Money)
			base := before.Field(i).Interface().(money.Money)
			if got != want(base) {
				assert.Failf(t, "test failed", "%s.%s was not scaled", scope, field.Name)
			}
			continue
		}
		if field.Type.Kind() == reflect.Slice && field.Type.Elem().Kind() == reflect.Struct {
			if before.Field(i).Len() != after.Field(i).Len() {
				require.FailNowf(t, "test failed", "%s.%s length changed", scope, field.Name)
			}
			for j := range before.Field(i).Len() {
				assertScaledMoneyFields(t, before.Field(i).Index(j), after.Field(i).Index(j),
					fmt.Sprintf("%s.%s[%d]", scope, field.Name, j), want)
			}
		}
	}
	if seen == 0 {
		require.FailNowf(t, "test failed", "%s has no money fields", scope)
	}
}

func TestResolveBilledAtUsesHistoricalRates(t *testing.T) {
	embedded := pricingpkg.EmbeddedGenAIDocument()
	resolver := NewPricingResolver([]EffectivePricingRow{{
		GenAI: embedded.Prices, GenAIVersion: embedded.Version,
		GenAISource: PricingRowSourceEmbedded,
	}})
	_, lookup, err := resolver.ResolveBilledAt(
		pricingpkg.PositAssistantProviderID, "gpt-5.6-luna", "gpt-5.6-luna",
		time.Date(2026, 7, 29, 23, 59, 59, 0, time.UTC),
	)
	if err != nil {
		require.FailNow(t, fmt.Sprint(err))
	}
	if lookup.Rates.InputPerMTok != money.MustParseDollars("1.1") {
		require.FailNowf(t, "test failed", "historical rate was not adjusted: %v", lookup.Rates.InputPerMTok)
	}
	if lookup.Adjustment != "posit-assistant@posit-assistant-v1" {
		require.FailNowf(t, "test failed", "unexpected adjustment identity: %q", lookup.Adjustment)
	}
}

func TestResolveBilledPreservesExactServiceBoundary(t *testing.T) {
	r := NewPricingResolver([]EffectivePricingRow{{ModelPattern: "m", Rates: ModelRates{OutputPerMTok: money.Money{Microdollars: 1_000_000}}}})
	_, posit, err := r.ResolveBilled(pricingpkg.PositAssistantProviderID, "m", "m")
	if err != nil {
		require.FailNow(t, fmt.Sprint(err))
	}
	_, other, err := r.ResolveBilled("positron", "m", "m")
	if err != nil {
		require.FailNow(t, fmt.Sprint(err))
	}
	_, unknown, err := r.ResolveBilled("unknown", "mystery", "mystery")
	if err != nil {
		require.FailNow(t, fmt.Sprint(err))
	}
	if posit.Rates.OutputPerMTok.Microdollars != 1_100_000 ||
		other.Rates.OutputPerMTok.Microdollars != 1_000_000 ||
		unknown.Rates.OutputPerMTok.Microdollars != 0 {
		require.FailNowf(t, "test failed", "unexpected rates: %v %v", posit.Rates.OutputPerMTok, other.Rates.OutputPerMTok)
	}
	t.Log("service boundaries: posit-assistant=1.1; positron=1.0; unknown=0.0")
}

func TestResolveBilledDoesNotDoubleAdjust(t *testing.T) {
	r := NewPricingResolver([]EffectivePricingRow{{ModelPattern: "m", Rates: ModelRates{OutputPerMTok: money.Money{Microdollars: 1_000_000}}}})
	_, first, err := r.ResolveBilled(pricingpkg.PositAssistantProviderID, "m", "m")
	if err != nil {
		require.FailNow(t, fmt.Sprint(err))
	}
	_, second, err := r.ResolveBilled(pricingpkg.PositAssistantProviderID, "m", "m")
	if err != nil {
		require.FailNow(t, fmt.Sprint(err))
	}
	if first.Rates.OutputPerMTok != second.Rates.OutputPerMTok || first.Rates.OutputPerMTok.Microdollars != 1_100_000 {
		require.FailNowf(t, "test failed", "adjustment was not stable: %v %v", first.Rates.OutputPerMTok, second.Rates.OutputPerMTok)
	}
}

func TestResolveBilledLeavesCustomModelRateUnchanged(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{{
		ModelPattern: "custom-model",
		Rates: ModelRates{
			InputPerMTok: money.MustParseDollars("2"),
			Source:       PricingRowSourceCustom,
		},
	}})
	_, lookup, err := resolver.ResolveBilled(
		pricingpkg.PositAssistantProviderID, "custom-model", "custom-model")
	if err != nil {
		require.FailNow(t, fmt.Sprint(err))
	}
	if lookup.Rates.InputPerMTok != money.MustParseDollars("2") {
		require.FailNowf(t, "test failed", "custom rate was adjusted: %v", lookup.Rates.InputPerMTok)
	}
}

func TestResolveBilledCodexServiceTiers(t *testing.T) {
	t.Run("GenAI dated alias", func(t *testing.T) {
		embedded := pricingpkg.EmbeddedGenAIDocument()
		resolver := NewPricingResolver([]EffectivePricingRow{
			{GenAI: embedded.Prices, GenAIVersion: embedded.Version, GenAISource: PricingRowSourceEmbedded},
			{ModelPattern: "gpt-5.6-luna", Rates: ModelRates{InputPerMTok: money.MustParseDollars("0.2"), Source: PricingRowSourceFetched}},
			{ModelPattern: "gpt-5.6-luna@priority", Rates: ModelRates{InputPerMTok: money.MustParseDollars("0.4"), Source: PricingRowSourceFetched}},
		})
		timestamp := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		for _, model := range []string{"gpt-5.6-luna", "gpt-5.6-luna-2026-07-13"} {
			t.Run(model, func(t *testing.T) {
				_, standard := resolver.ResolveAt(model, model, timestamp)
				require.True(t, standard.OK)
				require.Equal(t, "openai/gpt-5.6-luna", standard.Pattern)
				require.Equal(t, PricingRowSourceEmbedded, standard.Rates.Source)
				require.Equal(t, money.MustParseDollars("0.2"), standard.Rates.InputPerMTok)

				_, lookup, err := resolver.ResolveBilledAt(pricingpkg.OpenAIPriorityProviderID, model, model, timestamp)
				require.NoError(t, err)
				require.True(t, lookup.OK)
				assert.Equal(t, money.MustParseDollars("0.4"), lookup.Rates.InputPerMTok, "priority input rate")
				assert.Equal(t, "gpt-5.6-luna@priority", lookup.Pattern)
				assert.Equal(t, pricingpkg.OpenAIPriorityProviderID, lookup.Adjustment)
				cost, err := lookup.Rates.CostForTokens(100_000, 0, 0, 0, 0, 0)
				require.NoError(t, err)
				assert.Equal(t, money.MustParseDollars("0.04"), cost, "priority cost for 100,000 input tokens")
			})
		}
	})

	for _, tc := range []struct {
		name, provider, model, input, pattern, adjustment string
		custom                                            bool
		requestInput                                      int
		bandInput, bandOutput, cost                       string
	}{
		{"priority", "openai-priority", "gpt-6.1-sol", "4", "gpt-6.1-sol@priority", "openai-priority", false, 0, "", "", ""},
		{"ultrafast", "openai-ultrafast", "gpt-6.1-sol", "12", "gpt-6.1-sol@ultrafast", "openai-ultrafast", false, 0, "", "", ""},
		{"missing tier", "openai-ultrafast", "gpt-5.5", "5", "gpt-5.5", "", false, 0, "", "", ""},
		{"custom", "openai-priority", "gpt-6.1-sol", "2", "gpt-6.1-sol", "", true, 0, "", "", ""},
		{"prefixed", "openai-priority", "openai/gpt-5.5", "12.5", "gpt-5.5@priority", "openai-priority", false, 0, "", "", ""},
		{"published tier band", "openai-priority", "gpt-6.1-sol", "4", "gpt-6.1-sol@priority", "openai-priority", false, 300_000, "9", "33", "3.03"},
		{"missing tier band uses standard band", "openai-priority", "gpt-5.5", "12.5", "gpt-5.5@priority", "openai-priority", false, 300_000, "10", "45", "3.45"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := func(pattern, input string) EffectivePricingRow {
				source := PricingRowSourceFetched
				if tc.custom && pattern == tc.model {
					source = PricingRowSourceCustom
				}
				rates := ModelRates{InputPerMTok: money.MustParseDollars(input), Source: source}
				switch pattern {
				case "gpt-6.1-sol":
					rates.OutputPerMTok = money.MustParseDollars("10")
					rates.Bands = []PricingBand{{AboveInputTokens: 272_000, InputPerMTok: money.MustParseDollars("4"), OutputPerMTok: money.MustParseDollars("15")}}
				case "gpt-6.1-sol@priority":
					rates.OutputPerMTok = money.MustParseDollars("20")
					rates.Bands = []PricingBand{{AboveInputTokens: 272_000, InputPerMTok: money.MustParseDollars("9"), OutputPerMTok: money.MustParseDollars("33")}}
				case "gpt-5.5":
					rates.OutputPerMTok = money.MustParseDollars("30")
					rates.Bands = []PricingBand{{AboveInputTokens: 272_000, InputPerMTok: money.MustParseDollars("10"), OutputPerMTok: money.MustParseDollars("45")}}
				case "gpt-5.5@priority":
					rates.OutputPerMTok = money.MustParseDollars("150")
				}
				return EffectivePricingRow{ModelPattern: pattern, Rates: rates}
			}
			resolver := NewPricingResolver([]EffectivePricingRow{
				row("gpt-6.1-sol", "2"), row("gpt-6.1-sol@priority", "4"), row("gpt-6.1-sol@ultrafast", "12"),
				row("gpt-5.5", "5"), row("gpt-5.5@priority", "12.5"),
			})
			_, lookup, err := resolver.ResolveBilledAt(tc.provider, tc.model, tc.model, time.Time{})
			require.NoError(t, err)
			require.True(t, lookup.OK)
			assert.Equal(t, money.MustParseDollars(tc.input), lookup.Rates.InputPerMTok)
			assert.Equal(t, tc.pattern, lookup.Pattern)
			assert.Equal(t, tc.adjustment, lookup.Adjustment)
			if tc.requestInput > 0 {
				selected := lookup.Rates.RatesForTokens(tc.requestInput, 0, 0)
				assert.Equal(t, money.MustParseDollars(tc.bandInput), selected.InputPerMTok, "request band input rate")
				assert.Equal(t, money.MustParseDollars(tc.bandOutput), selected.OutputPerMTok, "request band output rate")
				cost, err := lookup.Rates.CostForTokens(tc.requestInput, 10_000, 0, 0, 0, 0)
				require.NoError(t, err)
				assert.Equal(t, money.MustParseDollars(tc.cost), cost, "request cost")
			}
		})
	}
}

func TestResolveBilledSortsEqualRateRecordsByAdjustment(t *testing.T) {
	resolver := NewPricingResolver([]EffectivePricingRow{{
		ModelPattern: "zero-model",
		Rates:        ModelRates{Source: PricingRowSourceFetched},
	}})
	_, base := resolver.ResolveAt("zero-model", "zero-model", time.Time{})
	resolver.RecordResolvedReported("zero-model", "zero-model", base)
	_, billed, err := resolver.ResolveBilled(
		pricingpkg.PositAssistantProviderID, "zero-model", "zero-model")
	if err != nil {
		require.FailNow(t, fmt.Sprint(err))
	}
	resolver.RecordResolvedComputed("zero-model", "zero-model", billed)

	block, err := resolver.BuildBlock()
	if err != nil {
		require.FailNow(t, fmt.Sprint(err))
	}
	resolutions := block.Models["zero-model"].Resolutions
	if len(resolutions) != 2 {
		require.FailNowf(t, "test failed", "got %d resolutions, want 2", len(resolutions))
	}
	if resolutions[0].CostSource != CostSourceReported ||
		resolutions[1].CostSource != CostSourceComputed {
		require.FailNowf(t, "test failed", "unexpected resolution order: %#v", resolutions)
	}
}
