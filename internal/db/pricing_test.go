package db

import (
	"context"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"

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
	d := testDB(t)

	rows, err := d.getReader().Query(
		`SELECT name FROM pragma_table_info('model_pricing_bands') ORDER BY cid`,
	)
	require.NoError(t, err)
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		columns = append(columns, column)
	}
	require.NoError(t, rows.Err())

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
	d := testDB(t)
	initial := ModelPricing{
		ModelPattern: "banded-model",
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []PricingBand{
			{AboveInputTokens: 272_000, InputPerMTok: money.MustParseDollars("4")},
			{AboveInputTokens: 200_000, InputPerMTok: money.MustParseDollars("2")},
		},
	}
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{initial}))

	got, err := d.GetModelPricing("banded-model")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Bands, 2)
	assert.Equal(t, 200_000, got.Bands[0].AboveInputTokens)
	assert.Equal(t, 272_000, got.Bands[1].AboveInputTokens)
	assert.NotEmpty(t, got.Bands[0].UpdatedAt)
	_, err = d.getWriter().Exec(`
		UPDATE model_pricing SET updated_at = ? WHERE model_pattern = ?`,
		"2000-01-01T00:00:00Z", "banded-model")
	require.NoError(t, err)

	updated := initial
	updated.Bands = []PricingBand{{
		AboveInputTokens: 200_000,
		InputPerMTok:     money.MustParseDollars("3"),
	}}
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{updated}))

	got, err = d.GetModelPricing("banded-model")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Bands, 1)
	assert.Equal(t, 200_000, got.Bands[0].AboveInputTokens)
	assert.Equal(t, money.MustParseDollars("3"), got.Bands[0].InputPerMTok)
	assert.NotEqual(t, "2000-01-01T00:00:00Z", got.UpdatedAt)
	firstRevision := got.UpdatedAt

	removed := initial
	removed.Bands = nil
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{removed}))
	got, err = d.GetModelPricing("banded-model")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got.Bands)
	assert.Greater(t, got.UpdatedAt, firstRevision)
}

func TestFilterChangedModelPricingDetectsPricingBandOnlyChange(t *testing.T) {
	existing := []ModelPricing{{
		ModelPattern: "model",
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.MustParseDollars("2"),
			UpdatedAt:        "2026-08-05T12:00:00Z",
		}},
	}}
	desired := []ModelPricing{{
		ModelPattern: "model",
		InputPerMTok: money.MustParseDollars("1"),
		Bands: []PricingBand{{
			AboveInputTokens: 200_000,
			InputPerMTok:     money.MustParseDollars("3"),
			UpdatedAt:        "2026-08-05T12:01:00Z",
		}},
	}}

	summary, changed := FilterChangedModelPricing(existing, desired)

	assert.Equal(t, PricingChangeSummary{Total: 1, Changed: 1}, summary)
	assert.Equal(t, desired, changed)
}

func TestUpsertModelPricing(t *testing.T) {
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
	require.NoError(t, err, "UpsertModelPricing")

	got, err := d.GetModelPricing("claude-sonnet-4")
	require.NoError(t, err, "GetModelPricing")
	require.NotNil(t, got, "expected pricing")

	assert.Equal(t, "claude-sonnet-4", got.ModelPattern)
	assert.Equal(t, money.MustParseDollars("3.0"), got.InputPerMTok)
	assert.Equal(t, money.MustParseDollars("15.0"), got.OutputPerMTok)
	assert.Equal(t, money.MustParseDollars("3.75"), got.CacheCreationPerMTok)
	assert.Equal(t, money.MustParseDollars("0.30"), got.CacheReadPerMTok)
	assert.NotEmpty(t, got.UpdatedAt, "expected UpdatedAt to be set")
}

func TestUpsertModelPricingRoundTrips1hCacheCreationRate(t *testing.T) {
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
	require.NoError(t, d.UpsertModelPricing(prices), "UpsertModelPricing")

	got, err := d.GetModelPricing("claude-fable-5")
	require.NoError(t, err, "GetModelPricing")
	require.NotNil(t, got, "expected pricing")
	assert.Equal(t, money.MustParseDollars("20.0"), got.CacheCreation1hPerMTok)
	require.Len(t, got.Bands, 1)
	assert.Equal(t, money.MustParseDollars("40.0"),
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
	require.NoError(t, err, "UpsertModelPricing initial")

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
	require.NoError(t, err, "UpsertModelPricing updated")

	got, err := d.GetModelPricing("claude-opus-4")
	require.NoError(t, err, "GetModelPricing after update")
	require.NotNil(t, got, "expected pricing")

	assert.Equal(t, money.MustParseDollars("10.0"), got.InputPerMTok)
	assert.Equal(t, money.MustParseDollars("50.0"), got.OutputPerMTok)
	assert.Equal(t, money.MustParseDollars("12.50"), got.CacheCreationPerMTok)
	assert.Equal(t, money.MustParseDollars("1.00"), got.CacheReadPerMTok)
}

func TestUpsertModelPricingAdvancesCollidingRevisionByOneMicrosecond(t *testing.T) {
	database := testDB(t)
	revision := "2099-01-01T00:00:00.123450Z"
	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "revision-collision",
		InputPerMTok: money.MustParseDollars("1"),
		UpdatedAt:    revision,
	}}))
	first, err := database.GetModelPricing("revision-collision")
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, revision, first.UpdatedAt)

	_, err = database.getWriter().Exec(`
		UPDATE model_pricing SET updated_at = ? WHERE model_pattern = ?`,
		revision, "revision-collision")
	require.NoError(t, err)

	require.NoError(t, database.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "revision-collision",
		InputPerMTok: money.MustParseDollars("2"),
		UpdatedAt:    revision,
	}}))

	stored, err := database.GetModelPricing("revision-collision")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "2099-01-01T00:00:00.123451Z", stored.UpdatedAt)
	assert.Greater(t, stored.UpdatedAt, first.UpdatedAt)
}

func TestFilterChangedModelPricingIgnoresUpdatedAtOnlyDifferences(t *testing.T) {
	existing := []ModelPricing{
		{
			ModelPattern:         "same-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("2"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "2026-08-05T12:00:00Z",
		},
		{
			ModelPattern:         "changed-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("2"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "2026-08-05T12:00:00Z",
		},
	}
	desired := []ModelPricing{
		{
			ModelPattern:         "same-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("2"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "2026-08-05T12:01:00Z",
		},
		{
			ModelPattern:         "changed-model",
			InputPerMTok:         money.MustParseDollars("1"),
			OutputPerMTok:        money.MustParseDollars("9"),
			CacheCreationPerMTok: money.MustParseDollars("3"),
			CacheReadPerMTok:     money.MustParseDollars("4"),
			UpdatedAt:            "2026-08-05T12:01:00Z",
		},
		{
			ModelPattern:         "missing-model",
			InputPerMTok:         money.MustParseDollars("5"),
			OutputPerMTok:        money.MustParseDollars("6"),
			CacheCreationPerMTok: money.MustParseDollars("7"),
			CacheReadPerMTok:     money.MustParseDollars("8"),
			UpdatedAt:            "2026-08-05T12:01:00Z",
		},
	}

	gotSummary, gotRows := FilterChangedModelPricing(existing, desired)

	assert.Equal(t, PricingChangeSummary{
		Total:     3,
		Missing:   1,
		Changed:   1,
		Unchanged: 1,
	}, gotSummary)
	require.Len(t, gotRows, 2)
	assert.Equal(t, "changed-model", gotRows[0].ModelPattern)
	assert.Equal(t, "missing-model", gotRows[1].ModelPattern)
}

func TestPricingMeta(t *testing.T) {
	d := testDB(t)

	// Initially empty.
	got, err := d.GetPricingMeta("_fallback_version")
	require.NoError(t, err, "GetPricingMeta empty")
	require.Empty(t, got)

	// Set and read back.
	require.NoError(t,
		d.SetPricingMeta("_fallback_version", "v1"),
		"SetPricingMeta v1")
	got, err = d.GetPricingMeta("_fallback_version")
	require.NoError(t, err, "GetPricingMeta v1")
	require.Equal(t, "v1", got)

	// Update overwrites.
	require.NoError(t,
		d.SetPricingMeta("_fallback_version", "v2"),
		"SetPricingMeta v2")
	got, err = d.GetPricingMeta("_fallback_version")
	require.NoError(t, err, "GetPricingMeta v2")
	require.Equal(t, "v2", got)

	var storedValue, metadataUpdatedAt string
	require.NoError(t, d.getReader().QueryRow(`
		SELECT value, updated_at FROM pricing_metadata
		WHERE key = '_fallback_version'`,
	).Scan(&storedValue, &metadataUpdatedAt))
	assert.Equal(t, "v2", storedValue)
	_, err = time.Parse(time.RFC3339Nano, metadataUpdatedAt)
	require.NoError(t, err, "pricing metadata updated_at must be a timestamp")

	var sentinelCount int
	require.NoError(t, d.getReader().QueryRow(`
		SELECT count(*) FROM model_pricing
		WHERE model_pattern = '_fallback_version'`,
	).Scan(&sentinelCount))
	assert.Zero(t, sentinelCount, "metadata must not create fake pricing rows")

	p, err := d.GetModelPricing("_fallback_version")
	require.NoError(t, err, "GetModelPricing metadata key")
	assert.Nil(t, p)
}

func TestReconcileModelPricing(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
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

	require.NoError(t, d.ReconcileModelPricing(
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
	require.NoError(t, err)
	assert.Nil(t, removed)
	var bands int
	require.NoError(t, d.getReader().QueryRow(
		`SELECT COUNT(*) FROM model_pricing_bands WHERE model_pattern = ?`,
		"minimax/minimax-m3",
	).Scan(&bands))
	assert.Zero(t, bands, "bands of a removed pattern are deleted")
	kept, err := d.GetModelPricing("acme/keep")
	require.NoError(t, err)
	require.NotNil(t, kept)
	assert.Equal(t, money.MustParseDollars("1"), kept.InputPerMTok)
	added, err := d.GetModelPricing("minimax/MiniMax-M3")
	require.NoError(t, err)
	require.NotNil(t, added)
	meta, err := d.GetPricingMeta("_openrouter_models")
	require.NoError(t, err)
	assert.Equal(t, `[]`, meta, "meta written with the rows")

	require.NoError(t, d.ReconcileModelPricing(
		nil, nil, PricingMeta{Key: "_openrouter_models", Value: `["x"]`},
	))
	meta, err = d.GetPricingMeta("_openrouter_models")
	require.NoError(t, err)
	assert.Equal(t, `["x"]`, meta, "meta-only reconcile still writes")
}

func TestPlanModelPricingSync(t *testing.T) {
	model := func(pattern, dollars string) ModelPricing {
		return ModelPricing{
			ModelPattern: pattern,
			InputPerMTok: money.MustParseDollars(dollars),
		}
	}
	tests := []struct {
		name       string
		existing   []ModelPricing
		desired    []ModelPricing
		targetMeta string
		localMeta  string
		wantChange []ModelPricing
		wantRemove []string
		wantMeta   PricingMeta
	}{
		{
			name:       "retired pattern removed and ownership dropped",
			existing:   []ModelPricing{model("minimax/minimax-m3", "9")},
			desired:    []ModelPricing{model("minimax/MiniMax-M3", "2")},
			targetMeta: `["minimax/minimax-m3"]`,
			localMeta:  `[]`,
			wantChange: []ModelPricing{model("minimax/MiniMax-M3", "2")},
			wantRemove: []string{"minimax/minimax-m3"},
			wantMeta: PricingMeta{
				Key: pricing.OpenRouterModelsMetaKey, Value: `[]`,
			},
		},
		{
			name:       "pattern adopted by a local non-OpenRouter row loses ownership",
			existing:   []ModelPricing{model("acme/model", "9")},
			desired:    []ModelPricing{model("acme/model", "9")},
			targetMeta: `["acme/model"]`,
			localMeta:  `[]`,
			wantChange: []ModelPricing{},
			wantMeta: PricingMeta{
				Key: pricing.OpenRouterModelsMetaKey, Value: `[]`,
			},
		},
		{
			name:       "delisted row another machine owns keeps its ownership",
			existing:   []ModelPricing{model("acme/delisted", "9")},
			desired:    []ModelPricing{model("acme/local", "1")},
			targetMeta: `["acme/delisted"]`,
			localMeta:  `["acme/local"]`,
			wantChange: []ModelPricing{model("acme/local", "1")},
			wantMeta: PricingMeta{
				Key:   pricing.OpenRouterModelsMetaKey,
				Value: `["acme/delisted","acme/local"]`,
			},
		},
		{
			name:     "local OpenRouter row covered by a target row is withheld",
			existing: []ModelPricing{model("acme/Stale-Model", "9")},
			desired: []ModelPricing{
				model("acme/stale-model", "5"),
				model("acme/fresh", "1"),
			},
			targetMeta: `[]`,
			localMeta:  `["acme/fresh","acme/stale-model"]`,
			wantChange: []ModelPricing{model("acme/fresh", "1")},
			wantMeta: PricingMeta{
				Key: pricing.OpenRouterModelsMetaKey, Value: `["acme/fresh"]`,
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
			changed, remove, meta, err := PlanModelPricingSync(
				tt.existing, tt.desired, tt.targetMeta, tt.localMeta,
			)
			require.NoError(t, err)
			assert.Equal(t, tt.wantChange, changed)
			assert.Equal(t, tt.wantRemove, remove)
			assert.Equal(t, tt.wantMeta, meta)
		})
	}
}

func TestPlanModelPricingSyncRejectsCorruptSentinel(t *testing.T) {
	_, _, _, err := PlanModelPricingSync(nil, nil, "not json", "")
	require.Error(t, err)
}

func TestGetModelPricingNotFound(t *testing.T) {
	d := testDB(t)

	got, err := d.GetModelPricing("nonexistent-model")
	require.NoError(t, err, "GetModelPricing not found")
	assert.Nil(t, got, "expected nil")
}

func TestInsertMissingModelPricing_DoesNotOverwrite(t *testing.T) {
	d := testDB(t)

	// Seed an existing row (simulating a LiteLLM rate already present).
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
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
	require.NoError(t, err, "InsertMissingModelPricing")

	// Existing row is untouched.
	opus, err := d.GetModelPricing("claude-opus-4-6")
	require.NoError(t, err, "GetModelPricing opus")
	require.NotNil(t, opus)
	assert.Equal(t, money.MustParseDollars("5.0"), opus.InputPerMTok, "opus InputPerMTok not overwritten")
	// New row was inserted.
	gpt, err := d.GetModelPricing("gpt-5.4")
	require.NoError(t, err, "GetModelPricing gpt")
	require.NotNil(t, gpt)
	assert.Equal(t, money.MustParseDollars("2.5"), gpt.InputPerMTok, "gpt-5.4 InputPerMTok inserted")
}

func TestInsertMissingModelPricingDoesNotAttachBandsToExistingFlatModel(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "existing-model",
		InputPerMTok: money.MustParseDollars("1"),
	}}))

	require.NoError(t, d.InsertMissingModelPricing([]ModelPricing{
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
	require.NoError(t, err)
	require.NotNil(t, existing)
	assert.Empty(t, existing.Bands)
	added, err := d.GetModelPricing("new-model")
	require.NoError(t, err)
	require.NotNil(t, added)
	require.Len(t, added.Bands, 1)
	assert.Equal(t, money.MustParseDollars("4"), added.Bands[0].InputPerMTok)
}

func TestLoadPricingMapKeepsCustomSourceWhenRatesMatchFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	fallback := fallbackRateMap()
	fallbackRates, ok := fallback["gpt-5.5"]
	require.True(t, ok, "expected gpt-5.5 fallback rates")
	d.SetCustomPricing(map[string]config.CustomModelRate{
		"gpt-5.5": {
			InputMicrodollarsPerMTok:         fallbackRates.InputPerMTok.Microdollars,
			OutputMicrodollarsPerMTok:        fallbackRates.OutputPerMTok.Microdollars,
			CacheCreationMicrodollarsPerMTok: fallbackRates.CacheWritePerMTok.Microdollars,
			CacheReadMicrodollarsPerMTok:     fallbackRates.CacheReadPerMTok.Microdollars,
		},
	})

	rows, err := d.LoadPricingMap(ctx)
	require.NoError(t, err, "loadPricingMap")
	resolver := export.NewPricingResolver(rows)
	lookup := resolver.Lookup("gpt-5.5")
	require.True(t, lookup.OK, "lookup custom fallback-rate row")
	assert.Equal(t, export.PricingRowSourceCustom,
		lookup.Rates.Source)
	assert.Empty(t, lookup.Rates.Bands)

	block, err := resolver.BuildBlock()
	require.NoError(t, err)
	assert.Equal(t, "custom+embedded", block.Source)
	assert.Equal(t, 1, block.CustomOverrideCount)
}

func TestDeleteModelPricing(t *testing.T) {
	d := testDB(t)

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{
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
	require.NoError(t, err, "DeleteModelPricing")

	for _, model := range []string{"kimi-for-coding", "daimon-kimi-code"} {
		row, err := d.GetModelPricing(model)
		require.NoError(t, err, "GetModelPricing %s", model)
		assert.Nil(t, row, "%s must be deleted", model)
	}

	// Untargeted rows survive.
	row, err := d.GetModelPricing("claude-opus-4-6")
	require.NoError(t, err, "GetModelPricing claude-opus-4-6")
	require.NotNil(t, row)
	assert.Equal(t, money.MustParseDollars("5.0"), row.InputPerMTok)

	// Deleting absent patterns and an empty list are no-ops.
	require.NoError(t,
		d.DeleteModelPricing([]string{"kimi-for-coding"}), "re-delete")
	require.NoError(t, d.DeleteModelPricing(nil), "empty delete")
}

func TestLoadPricingMapTreatsBandOnlyFallbackMismatchAsFetched(t *testing.T) {
	d := testDB(t)
	fallback, ok := fallbackRateMap()["gpt-5.5"]
	require.True(t, ok)
	require.NotEmpty(t, fallback.Bands)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "gpt-5.5",
		InputPerMTok:         fallback.InputPerMTok,
		OutputPerMTok:        fallback.OutputPerMTok,
		CacheCreationPerMTok: fallback.CacheWritePerMTok,
		CacheReadPerMTok:     fallback.CacheReadPerMTok,
	}}))

	rows, err := d.LoadPricingMap(context.Background())
	require.NoError(t, err)
	lookup := export.NewPricingResolver(rows).Lookup("gpt-5.5")
	require.True(t, lookup.OK)

	assert.Equal(t, export.PricingRowSourceFetched, lookup.Rates.Source)
	assert.Empty(t, lookup.Rates.Bands)
}
