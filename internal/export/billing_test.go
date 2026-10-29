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
	"go.kenn.io/agentsview/internal/pricing/catalog"
)

const codexLunaGenAIProvider = `{
    "id": "openai",
    "name": "OpenAI",
    "api_pattern": "https://example.invalid",
    "model_match": {"starts_with": "gpt-"},
    "provider_match": {"contains": "openai"},
    "models": [{
        "id": "gpt-5.6-luna",
        "match": {"or": [
            {"equals": "gpt-5.6-luna"},
            {"regex": "^gpt-5\\.6-luna-\\d{4}-\\d{2}-\\d{2}$"}
        ]},
        "prices": [
            {"prices": {"input_mtok": 1}},
            {"constraint": {"start_date": "2026-07-30"}, "prices": {"input_mtok": 0.2}}
        ]
    }]
}`

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
	t.Run("GenAI alias uses matching LiteLLM priority row", func(t *testing.T) {
		const model = "gpt-5.3-chat-latest"
		rows := embeddedCodexPricingRows(t, model)
		resolver := NewPricingResolver(rows)

		pricedModel, lookup, err := resolver.ResolveBilledAt(
			pricingpkg.OpenAIPriorityProviderID,
			model, model,
			time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		)
		require.NoError(t, err)
		assert.Equal(t, model, pricedModel)
		assert.Equal(t, money.MustParseDollars("3.5"), lookup.Rates.InputPerMTok)
		assert.Equal(t, money.MustParseDollars("28"), lookup.Rates.OutputPerMTok)
		assert.Equal(t, model+"@priority", lookup.Pattern)
		assert.Equal(t, pricingpkg.OpenAIPriorityProviderID, lookup.Adjustment)
	})

	t.Run("reported dated alias takes precedence", func(t *testing.T) {
		const model = "gpt-4o-2024-05-13"
		rows := embeddedCodexPricingRows(t, model, "gpt-4o")
		resolver := NewPricingResolver(rows)
		at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

		_, lookup, err := resolver.ResolveBilledAt(
			pricingpkg.OpenAIPriorityProviderID, model, model, at)
		require.NoError(t, err)
		assert.Equal(t, model+"@priority", lookup.Pattern)
		assert.Equal(t, money.MustParseDollars("8.75"), lookup.Rates.InputPerMTok)
		cost, err := lookup.Rates.CostForTokens(1_000_000, 0, 0, 0, 0, 0)
		require.NoError(t, err)
		assert.Equal(t, money.MustParseDollars("8.75"), cost)

		before, err := resolver.DependencyFingerprint(
			pricingpkg.OpenAIPriorityProviderID, model, model)
		require.NoError(t, err)
		changedRows := append([]EffectivePricingRow(nil), rows...)
		for i := range changedRows {
			if changedRows[i].ModelPattern == model+"@priority" {
				changedRows[i].Rates.InputPerMTok = money.MustParseDollars("9")
			}
		}
		after, err := NewPricingResolver(changedRows).DependencyFingerprint(
			pricingpkg.OpenAIPriorityProviderID, model, model)
		require.NoError(t, err)
		assert.NotEqual(t, before, after, "the selected priority row affects cached costs")
	})

	t.Run("GenAI dated alias", func(t *testing.T) {
		genAI, err := pricingpkg.ParseGenAIPrices([]byte("[" + codexLunaGenAIProvider + "]"))
		require.NoError(t, err)
		resolver := NewPricingResolver([]EffectivePricingRow{
			{GenAI: genAI, GenAIVersion: genAI.Version(), GenAISource: PricingRowSourceEmbedded},
			{ModelPattern: "gpt-5.6-luna", Rates: ModelRates{InputPerMTok: money.MustParseDollars("0.3"), Source: PricingRowSourceFetched}},
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
			})
		}
	})

	for _, tc := range []struct {
		name, provider, model, input, pattern, adjustment string
		custom                                            bool
		requestInput                                      int
		bandInput, bandOutput                             string
	}{
		{"missing tier", "openai-priority", "standard-only", "5", "standard-only", "", false, 0, "", ""},
		{"custom", "openai-priority", "gpt-6.1-sol", "2", "gpt-6.1-sol", "", true, 0, "", ""},
		{"prefixed", "openai-priority", "openai/gpt-5.5", "12.5", "gpt-5.5@priority", "openai-priority", false, 0, "", ""},
		{"published tier band", "openai-priority", "gpt-6.1-sol", "4", "gpt-6.1-sol@priority", "openai-priority", false, 300_000, "9", "33"},
		{"missing tier band falls back to standard band", "openai-priority", "gpt-5.5", "12.5", "gpt-5.5@priority", "openai-priority", false, 300_000, "10", "45"},
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
				row("gpt-6.1-sol", "2"), row("gpt-6.1-sol@priority", "4"),
				row("gpt-5.5", "5"), row("gpt-5.5@priority", "12.5"), row("standard-only", "5"),
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
			}
		})
	}
}

func embeddedCodexPricingRows(t *testing.T, models ...string) []EffectivePricingRow {
	t.Helper()

	embedded := pricingpkg.EmbeddedGenAIDocument()
	rows := []EffectivePricingRow{{
		GenAI: embedded.Prices, GenAIVersion: embedded.Version,
		GenAISource: PricingRowSourceEmbedded,
	}}
	wanted := make(map[string]struct{}, len(models)*2)
	for _, model := range models {
		wanted[model] = struct{}{}
		wanted[model+"@priority"] = struct{}{}
	}
	found := make(map[string]struct{}, len(wanted))
	for _, row := range pricingpkg.FallbackPricing() {
		if _, ok := wanted[row.ModelPattern]; !ok {
			continue
		}
		found[row.ModelPattern] = struct{}{}
		rows = append(rows, EffectivePricingRow{
			ModelPattern: row.ModelPattern,
			Rates: ModelRates{
				InputPerMTok: row.InputPerMTok, OutputPerMTok: row.OutputPerMTok,
				CacheWritePerMTok:   row.CacheCreationPerMTok,
				CacheWrite1hPerMTok: row.CacheCreation1hPerMTok,
				CacheReadPerMTok:    row.CacheReadPerMTok,
				Source:              PricingRowSourceFetched,
			},
		})
	}
	for pattern := range wanted {
		require.Contains(t, found, pattern)
	}
	return rows
}

func TestResolveBilledCodexServiceTierSparseBand(t *testing.T) {
	parsed, err := catalog.ParseLiteLLMPricing([]byte(`{
		"gpt-6.1-sol": {
			"litellm_provider": "openai",
			"input_cost_per_token": 0.000002,
			"output_cost_per_token": 0.00001,
			"cache_read_input_token_cost": 0.0000001,
			"input_cost_per_token_above_272k_tokens": 0.000004,
			"input_cost_per_token_priority": 0.000004,
			"output_cost_per_token_priority": 0.00002,
			"cache_read_input_token_cost_priority": 0.0000002
		}
	}`))
	require.NoError(t, err)
	rows := make([]EffectivePricingRow, 0, len(parsed))
	for _, price := range parsed {
		rates := ModelRates{
			InputPerMTok:     price.InputPerMTok,
			OutputPerMTok:    price.OutputPerMTok,
			CacheReadPerMTok: price.CacheReadPerMTok,
			Source:           PricingRowSourceFetched,
		}
		for _, band := range price.Bands {
			rates.Bands = append(rates.Bands, PricingBand{
				AboveInputTokens: band.AboveInputTokens,
				InputPerMTok:     band.InputPerMTok,
				OutputPerMTok:    band.OutputPerMTok,
				CacheReadPerMTok: band.CacheReadPerMTok,
			})
		}
		rows = append(rows, EffectivePricingRow{ModelPattern: price.ModelPattern, Rates: rates})
	}
	resolver := NewPricingResolver(rows)
	_, lookup, err := resolver.ResolveBilledAt(
		pricingpkg.OpenAIPriorityProviderID, "gpt-6.1-sol", "gpt-6.1-sol", time.Time{})
	require.NoError(t, err)
	cost, err := lookup.Rates.CostForTokens(300_000, 1_000_000, 0, 0, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("21.2"), cost)
	assert.Equal(t, money.MustParseDollars("0.2"),
		lookup.Rates.RatesForTokens(300_000, 0, 0).CacheReadPerMTok)
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
