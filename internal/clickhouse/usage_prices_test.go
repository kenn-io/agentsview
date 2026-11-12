package clickhouse

import (
	"encoding/json/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
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

func TestUsageLookupModelResolvesCodexAutoReview(t *testing.T) {
	assert.Equal(t, "gpt-5.6-luna",
		chUsageLookupModel("codex-auto-review", "2026-10-01T12:00:00Z"))
	assert.Equal(t, "gpt-5.6-luna",
		chUsageLookupModel("openai/codex-auto-review", ""))
}

func usagePriceTestDigest(
	t *testing.T, rows []export.EffectivePricingRow, models ...chUsagePriceModel,
) string {
	t.Helper()
	digest, err := chUsagePricingDigest(export.NewPricingResolver(rows), models)
	require.NoError(t, err)
	return digest
}

func TestUsagePricingDigestIgnoresRowTimestamps(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	model := chUsagePriceModel{model: "tier-test", priceModel: "tier-test"}
	base := usagePriceTestDigest(t, usagePriceTestRows("1", first), model)
	republished := usagePriceTestDigest(t, usagePriceTestRows("1", first.Add(time.Hour)), model)
	repriced := usagePriceTestDigest(t, usagePriceTestRows("1.5", first), model)

	assert.Equal(t, base, republished)
	assert.NotEqual(t, base, repriced)
}

// The digest names records priced for the registered models, so it moves
// with what those models resolve to and with nothing else in the catalog.
func TestUsagePricingDigestCoversOnlyRegisteredModels(t *testing.T) {
	updatedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	row := func(pattern, input string) export.EffectivePricingRow {
		return export.EffectivePricingRow{
			ModelPattern: pattern,
			Rates: export.ModelRates{
				InputPerMTok:  money.MustParseDollars(input),
				OutputPerMTok: money.MustParseDollars("10"),
				UpdatedAt:     &updatedAt,
				Source:        export.PricingRowSourceFetched,
			},
		}
	}
	used := chUsagePriceModel{model: "used-model", priceModel: "used-model"}
	other := chUsagePriceModel{providerID: "openai", model: "other-model", priceModel: "other-model"}
	baseRows := []export.EffectivePricingRow{row("used-model", "1"), row("unused-model", "2")}
	base := usagePriceTestDigest(t, baseRows, used)

	tests := []struct {
		name   string
		rows   []export.EffectivePricingRow
		models []chUsagePriceModel
		same   bool
	}{
		{
			name:   "unused row repriced",
			rows:   []export.EffectivePricingRow{row("used-model", "1"), row("unused-model", "3")},
			models: []chUsagePriceModel{used},
			same:   true,
		},
		{
			name:   "unused row added",
			rows:   append(slices.Clone(baseRows), row("brand-new-model", "4")),
			models: []chUsagePriceModel{used},
			same:   true,
		},
		{
			name:   "unused row removed",
			rows:   []export.EffectivePricingRow{row("used-model", "1")},
			models: []chUsagePriceModel{used},
			same:   true,
		},
		{
			name:   "registered model repeated",
			rows:   baseRows,
			models: []chUsagePriceModel{used, used},
			same:   true,
		},
		{
			name:   "used row repriced",
			rows:   []export.EffectivePricingRow{row("used-model", "1.5"), row("unused-model", "2")},
			models: []chUsagePriceModel{used},
		},
		{
			name:   "used row removed",
			rows:   []export.EffectivePricingRow{row("unused-model", "2")},
			models: []chUsagePriceModel{used},
		},
		{
			name:   "model registered",
			rows:   baseRows,
			models: []chUsagePriceModel{used, other},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := usagePriceTestDigest(t, tt.rows, tt.models...)
			if tt.same {
				assert.Equal(t, base, got)
			} else {
				assert.NotEqual(t, base, got)
			}
		})
	}
}

func TestUsagePricingDigestConfiguredAliases(t *testing.T) {
	t.Cleanup(func() { pricingpkg.SetConfiguredAliases(nil) })
	rows := usagePriceTestRows("1", time.Time{})
	model := chUsagePriceModel{model: "deployment", priceModel: "deployment"}
	before := usagePriceTestDigest(t, rows, model)
	pricingpkg.SetConfiguredAliases(map[string]string{"deployment": "tier-test"})
	mapped := usagePriceTestDigest(t, rows, model)
	assert.NotEqual(t, before, mapped)
	pricingpkg.SetConfiguredAliases(map[string]string{"deployment": "tier-test", "unrelated": "other"})
	assert.Equal(t, mapped, usagePriceTestDigest(t, rows, model))
	pricingpkg.SetConfiguredAliases(nil)
	assert.Equal(t, before, usagePriceTestDigest(t, rows, model))
}

// A new row can take over a registered model's resolution without touching
// the row it resolved to before.
func TestUsagePricingDigestFollowsResolutionToNewRow(t *testing.T) {
	updatedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := usagePriceTestRows("1", updatedAt)
	model := chUsagePriceModel{model: "tier-test-high", priceModel: "tier-test-high"}
	require.Equal(t, "tier-test", export.NewPricingResolver(rows).Lookup(model.model).Pattern)
	exact := rows[0]
	exact.ModelPattern = model.model
	withExact := append(slices.Clone(rows), exact)
	require.Equal(t, model.model, export.NewPricingResolver(withExact).Lookup(model.model).Pattern)

	assert.NotEqual(t,
		usagePriceTestDigest(t, rows, model),
		usagePriceTestDigest(t, withExact, model))
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

// BenchmarkUsagePricingDigest digests the embedded catalog for as many
// models as a long-lived mirror prices. Every push and every reader catalog
// reload pays this.
func BenchmarkUsagePricingDigest(b *testing.B) {
	embedded := db.EmbeddedGenAIPricingDocument()
	genAI, err := genAIEffectivePricingRow(&embedded)
	require.NoError(b, err)
	rows := append(db.MirrorPricingRows(db.FallbackPricingMap()), genAI)
	patterns := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.ModelPattern != "" {
			patterns = append(patterns, row.ModelPattern)
		}
	}
	slices.Sort(patterns)
	models := make([]chUsagePriceModel, 0, 50)
	for i := 0; len(models) < cap(models); i += len(patterns) / cap(models) {
		models = append(models, chUsagePriceModel{model: patterns[i], priceModel: patterns[i]})
	}
	b.Logf("%d catalog rows, %d models", len(rows), len(models))
	b.ReportAllocs()
	for b.Loop() {
		_, err := chUsagePricingDigest(export.NewPricingResolver(rows), models)
		require.NoError(b, err)
	}
}

// genAIHistoryDocument prices gpt-hist-test and gpt-other-test at their
// base input rates until 2026-01-13 and at 3 dollars per million input
// tokens from then on.
func genAIHistoryDocument(t *testing.T, histBase, otherBase string) db.GenAIPricingDocument {
	t.Helper()
	model := func(id, base string) string {
		return `{"id": "` + id + `", "match": {"equals": "` + id + `"}, "prices": [
			{"prices": {"input_mtok": ` + base + `, "output_mtok": 1}},
			{"constraint": {"start_date": "2026-01-13"}, "prices": {"input_mtok": 3, "output_mtok": 1}}
		]}`
	}
	data := []byte(`[{
		"id": "openai", "name": "OpenAI", "api_pattern": "https://example.invalid",
		"model_match": {"starts_with": "gpt-"},
		"models": [` + model("gpt-hist-test", histBase) + `, ` + model("gpt-other-test", otherBase) + `]
	}]`)
	prices, err := pricingpkg.ParseGenAIPrices(data)
	require.NoError(t, err)
	return db.GenAIPricingDocument{
		Version: prices.Version(), SourceRef: "test",
		Source: db.GenAIPricingSourceFetched, Data: data,
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
}

func genAIHistoryRows(t *testing.T, histBase, otherBase string) []export.EffectivePricingRow {
	t.Helper()
	document := genAIHistoryDocument(t, histBase, otherBase)
	genAI, err := genAIEffectivePricingRow(&document)
	require.NoError(t, err)
	return []export.EffectivePricingRow{genAI}
}

// A query for a past day prices each row at the rate in effect on that day,
// so a change to a past price period of a priced model must change the
// digest and reprice the mirror. The same change for an unpriced model must
// not.
func TestUsagePricingDigestFollowsPastPricePeriods(t *testing.T) {
	used := chUsagePriceModel{model: "gpt-hist-test", priceModel: "gpt-hist-test"}
	before := time.Date(2026, 1, 12, 12, 0, 0, 0, time.UTC)
	after := time.Date(2026, 1, 14, 12, 0, 0, 0, time.UTC)
	resolver := export.NewPricingResolver(genAIHistoryRows(t, "1", "1"))
	_, past := resolver.ResolveAt(used.model, used.priceModel, before)
	_, current := resolver.ResolveAt(used.model, used.priceModel, after)
	require.Equal(t, money.MustParseDollars("1"), past.Rates.InputPerMTok)
	require.Equal(t, money.MustParseDollars("3"), current.Rates.InputPerMTok)

	base := usagePriceTestDigest(t, genAIHistoryRows(t, "1", "1"), used)
	assert.NotEqual(t, base, usagePriceTestDigest(t, genAIHistoryRows(t, "2", "1"), used),
		"a priced model's past rate changed")
	assert.Equal(t, base, usagePriceTestDigest(t, genAIHistoryRows(t, "1", "2"), used),
		"only an unpriced model's past rate changed")
}

// A registered model whose provider adjustment overflows keeps the digest
// computable, and the digest still follows that model's rates.
func TestUsagePricingDigestSurvivesBillingOverflow(t *testing.T) {
	updatedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := func(microdollars int64) []export.EffectivePricingRow {
		return []export.EffectivePricingRow{{
			ModelPattern: "huge-model",
			Rates: export.ModelRates{
				InputPerMTok: money.Money{Microdollars: microdollars},
				UpdatedAt:    &updatedAt,
				Source:       export.PricingRowSourceFetched,
			},
		}}
	}
	model := chUsagePriceModel{
		providerID: pricingpkg.PositAssistantProviderID,
		model:      "huge-model", priceModel: "huge-model",
	}
	_, err := export.NewPricingResolver(rows(9_000_000_000_000_000_000)).
		DependencyFingerprint(model.providerID, model.model, model.priceModel)
	require.ErrorIs(t, err, money.ErrOverflow)

	overflowing := usagePriceTestDigest(t, rows(9_000_000_000_000_000_000), model)
	assert.NotEqual(t, overflowing,
		usagePriceTestDigest(t, rows(8_500_000_000_000_000_000), model),
		"another overflowing rate changes the records' contexts")
	assert.NotEqual(t, overflowing, usagePriceTestDigest(t, rows(1_000_000), model))
}
