package db

import (
	"testing"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationCreatesModelPricingTable(t *testing.T) {
	d := testDB(t)

	var count int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM pragma_table_info('model_pricing')`,
	).Scan(&count)
	require.NoError(t, err, "pragma_table_info")

	assert.NotZero(t, count, "model_pricing table not created by schema")
}

func TestMigrationCreatesModelPricingBandsTable(t *testing.T) {
	require := require.New(t)

	d := testDB(t)

	rows, err := d.getReader().Query(
		`SELECT name FROM pragma_table_info('model_pricing_bands') ORDER BY cid`,
	)
	require.NoError(err)
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		require.NoError(rows.Scan(&column))
		columns = append(columns, column)
	}
	require.NoError(rows.Err())

	assert.Equal(t, []string{
		"model_pattern",
		"above_input_tokens",
		"input_microdollars_per_mtok",
		"output_microdollars_per_mtok",
		"cache_creation_microdollars_per_mtok",
		"cache_creation_1h_microdollars_per_mtok",
		"cache_read_microdollars_per_mtok",
		"updated_at",
	}, columns)
}

func TestUpsertModelPricingPricingBandsReplacesCompleteSet(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	initial := ModelPricing{
		ModelPattern: "banded-model",
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []PricingBand{
			{AboveInputTokens: 272_000, InputPerMTok: money.MustParseDollars("4")},
			{AboveInputTokens: 200_000, InputPerMTok: money.MustParseDollars("2")},
		},
	}
	require.NoError(d.UpsertModelPricing([]ModelPricing{initial}))

	got, err := d.GetModelPricing("banded-model")
	require.NoError(err)
	require.NotNil(got)
	require.Len(got.Bands, 2)
	assert.Equal(200_000, got.Bands[0].AboveInputTokens)
	assert.Equal(272_000, got.Bands[1].AboveInputTokens)
	assert.NotEmpty(got.Bands[0].UpdatedAt)
	require.NoError(d.SetPricingMeta("banded-model", "2000-01-01T00:00:00Z"))

	updated := initial
	updated.Bands = []PricingBand{{
		AboveInputTokens: 200_000,
		InputPerMTok:     money.MustParseDollars("3"),
	}}
	require.NoError(d.UpsertModelPricing([]ModelPricing{updated}))

	got, err = d.GetModelPricing("banded-model")
	require.NoError(err)
	require.NotNil(got)
	require.Len(got.Bands, 1)
	assert.Equal(200_000, got.Bands[0].AboveInputTokens)
	assert.Equal(money.MustParseDollars("3"), got.Bands[0].InputPerMTok)
	assert.NotEqual("2000-01-01T00:00:00Z", got.UpdatedAt)
	firstRevision := got.UpdatedAt

	removed := initial
	removed.Bands = nil
	require.NoError(d.UpsertModelPricing([]ModelPricing{removed}))
	got, err = d.GetModelPricing("banded-model")
	require.NoError(err)
	require.NotNil(got)
	assert.Empty(got.Bands)
	assert.Greater(got.UpdatedAt, firstRevision)
}

func TestFilterChangedModelPricingDetectsPricingBandOnlyChange(t *testing.T) {
	existing := []ModelPricing{{
		ModelPattern: "model",
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.MustParseDollars("2"),
			UpdatedAt:        "old",
		}},
	}}
	desired := []ModelPricing{{
		ModelPattern: "model",
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.MustParseDollars("3"),
			UpdatedAt:        "new",
		}},
	}}

	summary, changed := FilterChangedModelPricing(existing, desired)

	assert.Equal(t, PricingChangeSummary{Total: 1, Changed: 1}, summary)
	assert.Equal(t, desired, changed)
}

func TestUpsertModelPricing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)

	prices := []ModelPricing{
		{
			ModelPattern:         "claude-sonnet-4",
			InputPerMTok:         money.MustParseDollars("3.0"),
			OutputPerMTok:        money.MustParseDollars("15.0"),
			CacheCreationPerMTok: money.MustParseDollars("3.75"),
			CacheReadPerMTok:     money.MustParseDollars("0.30"),
		},
	}

	err := d.UpsertModelPricing(prices)
	require.NoError(err, "UpsertModelPricing")

	got, err := d.GetModelPricing("claude-sonnet-4")
	require.NoError(err, "GetModelPricing")
	require.NotNil(got, "expected pricing")

	assert.Equal("claude-sonnet-4", got.ModelPattern)
	assert.Equal(money.MustParseDollars("3.0"), got.InputPerMTok)
	assert.Equal(money.MustParseDollars("15.0"), got.OutputPerMTok)
	assert.Equal(money.MustParseDollars("3.75"), got.CacheCreationPerMTok)
	assert.Equal(money.MustParseDollars("0.30"), got.CacheReadPerMTok)
	assert.NotEmpty(got.UpdatedAt, "expected UpdatedAt to be set")
}

func TestUpsertModelPricingRoundTrips1hCacheCreationRate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)

	prices := []ModelPricing{{
		ModelPattern:           "claude-fable-5",
		InputPerMTok:           money.MustParseDollars("10.0"),
		OutputPerMTok:          money.MustParseDollars("50.0"),
		CacheCreationPerMTok:   money.MustParseDollars("12.50"),
		CacheCreation1hPerMTok: money.MustParseDollars("20.0"),
		CacheReadPerMTok:       money.MustParseDollars("1.00"),
		Bands: []PricingBand{{
			AboveInputTokens:       200_000,
			InputPerMTok:           money.MustParseDollars("20.0"),
			OutputPerMTok:          money.MustParseDollars("100.0"),
			CacheCreationPerMTok:   money.MustParseDollars("25.0"),
			CacheCreation1hPerMTok: money.MustParseDollars("40.0"),
			CacheReadPerMTok:       money.MustParseDollars("2.00"),
		}},
	}}
	require.NoError(d.UpsertModelPricing(prices), "UpsertModelPricing")

	got, err := d.GetModelPricing("claude-fable-5")
	require.NoError(err, "GetModelPricing")
	require.NotNil(got, "expected pricing")
	assert.Equal(money.MustParseDollars("20.0"), got.CacheCreation1hPerMTok)
	require.Len(got.Bands, 1)
	assert.Equal(money.MustParseDollars("40.0"),
		got.Bands[0].CacheCreation1hPerMTok)
}

func TestFilterChangedModelPricingDetects1hRateOnlyChange(t *testing.T) {
	existing := []ModelPricing{{
		ModelPattern:         "model",
		InputPerMTok:         money.MustParseDollars("1"),
		CacheCreationPerMTok: money.MustParseDollars("1.25"),
	}}
	desired := []ModelPricing{{
		ModelPattern:           "model",
		InputPerMTok:           money.MustParseDollars("1"),
		CacheCreationPerMTok:   money.MustParseDollars("1.25"),
		CacheCreation1hPerMTok: money.MustParseDollars("2"),
	}}

	summary, changed := FilterChangedModelPricing(existing, desired)

	assert.Equal(t, PricingChangeSummary{Total: 1, Changed: 1}, summary)
	assert.Equal(t, desired, changed)
}

func TestUpsertModelPricingOverwrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)

	initial := []ModelPricing{
		{
			ModelPattern:         "claude-opus-4",
			InputPerMTok:         money.MustParseDollars("15.0"),
			OutputPerMTok:        money.MustParseDollars("75.0"),
			CacheCreationPerMTok: money.MustParseDollars("18.75"),
			CacheReadPerMTok:     money.MustParseDollars("1.50"),
		},
	}
	err := d.UpsertModelPricing(initial)
	require.NoError(err, "UpsertModelPricing initial")

	updated := []ModelPricing{
		{
			ModelPattern:         "claude-opus-4",
			InputPerMTok:         money.MustParseDollars("10.0"),
			OutputPerMTok:        money.MustParseDollars("50.0"),
			CacheCreationPerMTok: money.MustParseDollars("12.50"),
			CacheReadPerMTok:     money.MustParseDollars("1.00"),
		},
	}
	err = d.UpsertModelPricing(updated)
	require.NoError(err, "UpsertModelPricing updated")

	got, err := d.GetModelPricing("claude-opus-4")
	require.NoError(err, "GetModelPricing after update")
	require.NotNil(got, "expected pricing")

	assert.Equal(money.MustParseDollars("10.0"), got.InputPerMTok)
	assert.Equal(money.MustParseDollars("50.0"), got.OutputPerMTok)
	assert.Equal(money.MustParseDollars("12.50"), got.CacheCreationPerMTok)
	assert.Equal(money.MustParseDollars("1.00"), got.CacheReadPerMTok)
}

// Model rows compare by rate; sentinel metadata rows keep their value in
// updated_at, so a new value is a change.
func TestFilterChangedModelPricingIgnoresUpdatedAtOnlyDifferences(t *testing.T) {
	assert := assert.New(t)

	existing := []ModelPricing{
		{
			ModelPattern:         "_fallback_version",
			InputPerMTok:         money.MustParseDollars("0"),
			OutputPerMTok:        money.MustParseDollars("0"),
			CacheCreationPerMTok: money.MustParseDollars("0"),
			CacheReadPerMTok:     money.MustParseDollars("0"),
			UpdatedAt:            "v1",
		},
		{
			ModelPattern:         "same-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("2"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "old",
		},
		{
			ModelPattern:         "changed-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("2"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "old",
		},
	}
	desired := []ModelPricing{
		{
			ModelPattern:         "_fallback_version",
			InputPerMTok:         money.MustParseDollars("0"),
			OutputPerMTok:        money.MustParseDollars("0"),
			CacheCreationPerMTok: money.MustParseDollars("0"),
			CacheReadPerMTok:     money.MustParseDollars("0"),
			UpdatedAt:            "v2",
		},
		{
			ModelPattern:         "same-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("2"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "new",
		},
		{
			ModelPattern:         "changed-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("9"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "new",
		},
		{
			ModelPattern:         "missing-model",
			InputPerMTok:         money.MustParseDollars("5"),
			OutputPerMTok:        money.MustParseDollars("6"),
			CacheCreationPerMTok: money.MustParseDollars("7"),
			CacheReadPerMTok:     money.MustParseDollars("8"),
			UpdatedAt:            "new",
		},
	}

	gotSummary, gotRows := FilterChangedModelPricing(existing, desired)

	assert.Equal(PricingChangeSummary{
		Total:     4,
		Missing:   1,
		Changed:   2,
		Unchanged: 1,
	}, gotSummary)
	require.Len(t, gotRows, 3)
	assert.Equal("_fallback_version", gotRows[0].ModelPattern)
	assert.Equal("changed-model", gotRows[1].ModelPattern)
	assert.Equal("missing-model", gotRows[2].ModelPattern)
}

func TestPricingMeta(t *testing.T) {
	require := require.New(t)

	d := testDB(t)

	// Initially empty.
	got, err := d.GetPricingMeta("_fallback_version")
	require.NoError(err, "GetPricingMeta empty")
	require.Empty(got)

	// Set and read back.
	require.NoError(d.SetPricingMeta("_fallback_version", "v1"),
		"SetPricingMeta v1")
	got, err = d.GetPricingMeta("_fallback_version")
	require.NoError(err, "GetPricingMeta v1")
	require.Equal("v1", got)

	// Update overwrites.
	require.NoError(d.SetPricingMeta("_fallback_version", "v2"),
		"SetPricingMeta v2")
	got, err = d.GetPricingMeta("_fallback_version")
	require.NoError(err, "GetPricingMeta v2")
	require.Equal("v2", got)

	// Sentinel row does not interfere with model lookups.
	p, err := d.GetModelPricing("_fallback_version")
	require.NoError(err, "GetModelPricing sentinel")
	if p != nil {
		assert.Zero(t, p.InputPerMTok,
			"sentinel should have zero pricing, got %+v", p)
	}
}

func TestReconcileModelPricing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	require.NoError(d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern: "minimax/minimax-m3",
			InputPerMTok: money.MustParseDollars("9"),
			Bands: []PricingBand{{
				AboveInputTokens: 1000,
				InputPerMTok:     money.MustParseDollars("10"),
			}},
		},
		{ModelPattern: "acme/keep", InputPerMTok: money.MustParseDollars("1")},
	}))

	require.NoError(d.ReconcileModelPricing(
		[]ModelPricing{
			{
				ModelPattern: "minimax/MiniMax-M3",
				InputPerMTok: money.MustParseDollars("2"),
			},
			// Removed and desired at once: the upsert wins.
			{ModelPattern: "acme/keep", InputPerMTok: money.MustParseDollars("1")},
		},
		[]string{"minimax/minimax-m3", "acme/keep"},
		PricingMeta{Key: "_openrouter_models", Value: `[]`},
	))

	removed, err := d.GetModelPricing("minimax/minimax-m3")
	require.NoError(err)
	assert.Nil(removed)
	var bands int
	require.NoError(d.getReader().QueryRow(
		`SELECT COUNT(*) FROM model_pricing_bands WHERE model_pattern = ?`,
		"minimax/minimax-m3",
	).Scan(&bands))
	assert.Zero(bands, "bands of a removed pattern are deleted")
	kept, err := d.GetModelPricing("acme/keep")
	require.NoError(err)
	require.NotNil(kept)
	assert.Equal(money.MustParseDollars("1"), kept.InputPerMTok)
	added, err := d.GetModelPricing("minimax/MiniMax-M3")
	require.NoError(err)
	require.NotNil(added)
	meta, err := d.GetPricingMeta("_openrouter_models")
	require.NoError(err)
	assert.Equal(`[]`, meta, "meta written with the rows")

	require.NoError(d.ReconcileModelPricing(
		nil, nil, PricingMeta{Key: "_openrouter_models", Value: `["x"]`},
	))
	meta, err = d.GetPricingMeta("_openrouter_models")
	require.NoError(err)
	assert.Equal(`["x"]`, meta, "meta-only reconcile still writes")
}

func TestPlanModelPricingSync(t *testing.T) {
	model := func(pattern, dollars string) ModelPricing {
		return ModelPricing{
			ModelPattern: pattern,
			InputPerMTok: money.MustParseDollars(dollars),
		}
	}
	meta := func(value string) ModelPricing {
		return ModelPricing{
			ModelPattern: "_openrouter_models", UpdatedAt: value,
		}
	}
	tests := []struct {
		name       string
		existing   []ModelPricing
		desired    []ModelPricing
		wantChange []ModelPricing
		wantRemove []string
	}{
		{
			name: "retired pattern removed and ownership dropped",
			existing: []ModelPricing{
				model("minimax/minimax-m3", "9"),
				meta(`["minimax/minimax-m3"]`),
			},
			desired: []ModelPricing{
				model("minimax/MiniMax-M3", "2"),
				meta(`[]`),
			},
			wantChange: []ModelPricing{
				model("minimax/MiniMax-M3", "2"), meta(`[]`),
			},
			wantRemove: []string{"minimax/minimax-m3"},
		},
		{
			name: "pattern adopted by a local non-OpenRouter row loses ownership",
			existing: []ModelPricing{
				model("acme/model", "9"),
				meta(`["acme/model"]`),
			},
			desired: []ModelPricing{
				model("acme/model", "9"),
				meta(`[]`),
			},
			wantChange: []ModelPricing{meta(`[]`)},
		},
		{
			name: "delisted row another machine owns keeps its ownership",
			existing: []ModelPricing{
				model("acme/delisted", "9"),
				meta(`["acme/delisted"]`),
			},
			desired: []ModelPricing{
				model("acme/local", "1"),
				meta(`["acme/local"]`),
			},
			wantChange: []ModelPricing{
				model("acme/local", "1"),
				meta(`["acme/delisted","acme/local"]`),
			},
		},
		{
			name: "local OpenRouter row covered by a target row is withheld",
			existing: []ModelPricing{
				model("acme/Stale-Model", "9"),
				meta(`[]`),
			},
			desired: []ModelPricing{
				model("acme/stale-model", "5"),
				model("acme/fresh", "1"),
				meta(`["acme/fresh","acme/stale-model"]`),
			},
			wantChange: []ModelPricing{
				model("acme/fresh", "1"),
				meta(`["acme/fresh"]`),
			},
		},
		{
			name:     "no sentinel anywhere pushes plain rows",
			existing: []ModelPricing{model("acme/model", "9")},
			desired:  []ModelPricing{model("other", "1")},
			wantChange: []ModelPricing{
				model("other", "1"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed, remove, err := PlanModelPricingSync(
				tt.existing, tt.desired,
			)
			require.NoError(t, err)
			assert.Equal(t, tt.wantChange, changed)
			assert.Equal(t, tt.wantRemove, remove)
		})
	}
}

func TestPlanModelPricingSyncRejectsCorruptSentinel(t *testing.T) {
	_, _, err := PlanModelPricingSync([]ModelPricing{{
		ModelPattern: "_openrouter_models", UpdatedAt: "not json",
	}}, nil)
	require.Error(t, err)
}

func TestGetModelPricingNotFound(t *testing.T) {
	d := testDB(t)

	got, err := d.GetModelPricing("nonexistent-model")
	require.NoError(t, err, "GetModelPricing not found")
	assert.Nil(t, got, "expected nil")
}

func TestInsertMissingModelPricing_DoesNotOverwrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)

	// Seed an existing row (simulating a LiteLLM rate already present).
	require.NoError(d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         money.MustParseDollars("5.0"),
		OutputPerMTok:        money.MustParseDollars("25.0"),
		CacheCreationPerMTok: money.MustParseDollars("6.25"),
		CacheReadPerMTok:     money.MustParseDollars("0.5"),
	}}), "UpsertModelPricing")

	// Insert-missing with a DIFFERENT rate for the same pattern, plus a
	// brand-new pattern.
	err := d.InsertMissingModelPricing([]ModelPricing{
		{ModelPattern: "claude-opus-4-6", InputPerMTok: money.MustParseDollars("999.0"), OutputPerMTok: money.MustParseDollars("999.0")},
		{ModelPattern: "gpt-5.4", InputPerMTok: money.MustParseDollars("2.5"), OutputPerMTok: money.MustParseDollars("15.0")},
	})
	require.NoError(err, "InsertMissingModelPricing")

	// Existing row is untouched.
	opus, err := d.GetModelPricing("claude-opus-4-6")
	require.NoError(err, "GetModelPricing opus")
	require.NotNil(opus)
	assert.Equal(money.MustParseDollars("5.0"), opus.InputPerMTok, "opus InputPerMTok not overwritten")
	// New row was inserted.
	gpt, err := d.GetModelPricing("gpt-5.4")
	require.NoError(err, "GetModelPricing gpt")
	require.NotNil(gpt)
	assert.Equal(money.MustParseDollars("2.5"), gpt.InputPerMTok, "gpt-5.4 InputPerMTok inserted")
}

func TestInsertMissingModelPricingDoesNotAttachBandsToExistingFlatModel(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	require.NoError(d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "existing-model",
		InputPerMTok: money.MustParseDollars("1"),
	}}))

	require.NoError(d.InsertMissingModelPricing([]ModelPricing{
		{
			ModelPattern: "existing-model",
			InputPerMTok: money.MustParseDollars("99"),
			Bands: []PricingBand{{
				AboveInputTokens: 200_000,
				InputPerMTok:     money.MustParseDollars("100"),
			}},
		},
		{
			ModelPattern: "new-model",
			InputPerMTok: money.MustParseDollars("2"),
			Bands: []PricingBand{{
				AboveInputTokens: 200_000,
				InputPerMTok:     money.MustParseDollars("4"),
			}},
		},
	}))

	existing, err := d.GetModelPricing("existing-model")
	require.NoError(err)
	require.NotNil(existing)
	assert.Empty(existing.Bands)
	added, err := d.GetModelPricing("new-model")
	require.NoError(err)
	require.NotNil(added)
	require.Len(added.Bands, 1)
	assert.Equal(money.MustParseDollars("4"), added.Bands[0].InputPerMTok)
}

func TestLoadPricingMapKeepsCustomSourceWhenRatesMatchFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	ctx := t.Context()

	fallback := fallbackRateMap()
	fallbackRates, ok := fallback["gpt-5.5"]
	require.True(ok, "expected gpt-5.5 fallback rates")
	d.SetCustomPricing(map[string]config.CustomModelRate{
		"gpt-5.5": {
			InputMicrodollarsPerMTok:         fallbackRates.InputPerMTok.Microdollars,
			OutputMicrodollarsPerMTok:        fallbackRates.OutputPerMTok.Microdollars,
			CacheCreationMicrodollarsPerMTok: fallbackRates.CacheWritePerMTok.Microdollars,
			CacheReadMicrodollarsPerMTok:     fallbackRates.CacheReadPerMTok.Microdollars,
		},
	})

	rows, err := d.loadPricingMap(ctx)
	require.NoError(err, "loadPricingMap")
	resolver := export.NewPricingResolver(rows)
	lookup := resolver.Lookup("gpt-5.5")
	require.True(lookup.OK, "lookup custom fallback-rate row")
	assert.Equal(export.PricingRowSourceCustom,
		lookup.Rates.Source)
	assert.Empty(lookup.Rates.Bands)

	block, err := resolver.BuildBlock()
	require.NoError(err)
	assert.Equal("custom+embedded", block.Source)
	assert.Equal(1, block.CustomOverrideCount)
}

func TestDeleteModelPricing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)

	require.NoError(d.UpsertModelPricing([]ModelPricing{
		{
			ModelPattern:  "kimi-for-coding",
			InputPerMTok:  money.MustParseDollars("0.95"),
			OutputPerMTok: money.MustParseDollars("4.0"),
		},
		{
			ModelPattern:  "daimon-kimi-code",
			InputPerMTok:  money.MustParseDollars("0.95"),
			OutputPerMTok: money.MustParseDollars("4.0"),
		},
		{
			ModelPattern:  "claude-opus-4-6",
			InputPerMTok:  money.MustParseDollars("5.0"),
			OutputPerMTok: money.MustParseDollars("25.0"),
		},
	}), "UpsertModelPricing")

	err := d.DeleteModelPricing([]string{"kimi-for-coding", "daimon-kimi-code"})
	require.NoError(err, "DeleteModelPricing")

	for _, model := range []string{"kimi-for-coding", "daimon-kimi-code"} {
		row, err := d.GetModelPricing(model)
		require.NoError(err, "GetModelPricing %s", model)
		assert.Nil(row, "%s must be deleted", model)
	}

	// Untargeted rows survive.
	row, err := d.GetModelPricing("claude-opus-4-6")
	require.NoError(err, "GetModelPricing claude-opus-4-6")
	require.NotNil(row)
	assert.Equal(money.MustParseDollars("5.0"), row.InputPerMTok)

	// Deleting absent patterns and an empty list are no-ops.
	require.NoError(d.DeleteModelPricing([]string{"kimi-for-coding"}), "re-delete")
	require.NoError(d.DeleteModelPricing(nil), "empty delete")
}

func TestLoadPricingMapTreatsBandOnlyFallbackMismatchAsFetched(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := testDB(t)
	fallback, ok := fallbackRateMap()["gpt-5.5"]
	require.True(ok)
	require.NotEmpty(fallback.Bands)
	require.NoError(d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "gpt-5.5",
		InputPerMTok:         fallback.InputPerMTok,
		OutputPerMTok:        fallback.OutputPerMTok,
		CacheCreationPerMTok: fallback.CacheWritePerMTok,
		CacheReadPerMTok:     fallback.CacheReadPerMTok,
	}}))

	rows, err := d.loadPricingMap(t.Context())
	require.NoError(err)
	lookup := export.NewPricingResolver(rows).Lookup("gpt-5.5")
	require.True(lookup.OK)

	assert.Equal(export.PricingRowSourceFetched, lookup.Rates.Source)
	assert.Empty(lookup.Rates.Bands)
}
