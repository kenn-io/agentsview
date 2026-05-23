package db

import (
	"testing"
)

func TestMigrationCreatesModelPricingTable(t *testing.T) {
	d := testDB(t)

	var count int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM pragma_table_info('model_pricing')`,
	).Scan(&count)
	requireNoError(t, err, "pragma_table_info")

	if count == 0 {
		t.Fatal("model_pricing table not created by schema")
	}
}

func TestUpsertModelPricing(t *testing.T) {
	d := testDB(t)

	prices := []ModelPricing{
		{
			ModelPattern:         "claude-sonnet-4",
			InputPerMTok:         3.0,
			OutputPerMTok:        15.0,
			CacheCreationPerMTok: 3.75,
			CacheReadPerMTok:     0.30,
		},
	}

	err := d.UpsertModelPricing(prices)
	requireNoError(t, err, "UpsertModelPricing")

	got, err := d.GetModelPricing("claude-sonnet-4")
	requireNoError(t, err, "GetModelPricing")

	if got == nil {
		t.Fatal("expected pricing, got nil")
	}
	if got.ModelPattern != "claude-sonnet-4" {
		t.Errorf("ModelPattern = %q, want %q",
			got.ModelPattern, "claude-sonnet-4")
	}
	if got.InputPerMTok != 3.0 {
		t.Errorf("InputPerMTok = %v, want 3.0",
			got.InputPerMTok)
	}
	if got.OutputPerMTok != 15.0 {
		t.Errorf("OutputPerMTok = %v, want 15.0",
			got.OutputPerMTok)
	}
	if got.CacheCreationPerMTok != 3.75 {
		t.Errorf("CacheCreationPerMTok = %v, want 3.75",
			got.CacheCreationPerMTok)
	}
	if got.CacheReadPerMTok != 0.30 {
		t.Errorf("CacheReadPerMTok = %v, want 0.30",
			got.CacheReadPerMTok)
	}
	if got.UpdatedAt == "" {
		t.Error("expected UpdatedAt to be set")
	}
}

func TestUpsertModelPricingOverwrites(t *testing.T) {
	d := testDB(t)

	initial := []ModelPricing{
		{
			ModelPattern:         "claude-opus-4",
			InputPerMTok:         15.0,
			OutputPerMTok:        75.0,
			CacheCreationPerMTok: 18.75,
			CacheReadPerMTok:     1.50,
		},
	}
	err := d.UpsertModelPricing(initial)
	requireNoError(t, err, "UpsertModelPricing initial")

	updated := []ModelPricing{
		{
			ModelPattern:         "claude-opus-4",
			InputPerMTok:         10.0,
			OutputPerMTok:        50.0,
			CacheCreationPerMTok: 12.50,
			CacheReadPerMTok:     1.00,
		},
	}
	err = d.UpsertModelPricing(updated)
	requireNoError(t, err, "UpsertModelPricing updated")

	got, err := d.GetModelPricing("claude-opus-4")
	requireNoError(t, err, "GetModelPricing after update")

	if got == nil {
		t.Fatal("expected pricing, got nil")
	}
	if got.InputPerMTok != 10.0 {
		t.Errorf("InputPerMTok = %v, want 10.0",
			got.InputPerMTok)
	}
	if got.OutputPerMTok != 50.0 {
		t.Errorf("OutputPerMTok = %v, want 50.0",
			got.OutputPerMTok)
	}
	if got.CacheCreationPerMTok != 12.50 {
		t.Errorf("CacheCreationPerMTok = %v, want 12.50",
			got.CacheCreationPerMTok)
	}
	if got.CacheReadPerMTok != 1.00 {
		t.Errorf("CacheReadPerMTok = %v, want 1.00",
			got.CacheReadPerMTok)
	}
}

func TestPricingMeta(t *testing.T) {
	d := testDB(t)

	// Initially empty.
	got, err := d.GetPricingMeta("_fallback_version")
	requireNoError(t, err, "GetPricingMeta empty")
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}

	// Set and read back.
	requireNoError(t,
		d.SetPricingMeta("_fallback_version", "v1"),
		"SetPricingMeta v1")
	got, err = d.GetPricingMeta("_fallback_version")
	requireNoError(t, err, "GetPricingMeta v1")
	if got != "v1" {
		t.Fatalf("expected %q, got %q", "v1", got)
	}

	// Update overwrites.
	requireNoError(t,
		d.SetPricingMeta("_fallback_version", "v2"),
		"SetPricingMeta v2")
	got, err = d.GetPricingMeta("_fallback_version")
	requireNoError(t, err, "GetPricingMeta v2")
	if got != "v2" {
		t.Fatalf("expected %q, got %q", "v2", got)
	}

	// Sentinel row does not interfere with model lookups.
	p, err := d.GetModelPricing("_fallback_version")
	requireNoError(t, err, "GetModelPricing sentinel")
	if p != nil && p.InputPerMTok != 0 {
		t.Errorf("sentinel should have zero pricing, got %+v", p)
	}
}

func TestGetModelPricingNotFound(t *testing.T) {
	d := testDB(t)

	got, err := d.GetModelPricing("nonexistent-model")
	requireNoError(t, err, "GetModelPricing not found")

	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestInsertMissingModelPricing_DoesNotOverwrite(t *testing.T) {
	d := testDB(t)

	// Seed an existing row (simulating a LiteLLM rate already present).
	if err := d.UpsertModelPricing([]ModelPricing{{
		ModelPattern:         "claude-opus-4-6",
		InputPerMTok:         5.0,
		OutputPerMTok:        25.0,
		CacheCreationPerMTok: 6.25,
		CacheReadPerMTok:     0.5,
	}}); err != nil {
		t.Fatalf("UpsertModelPricing: %v", err)
	}

	// Insert-missing with a DIFFERENT rate for the same pattern, plus a
	// brand-new pattern.
	err := d.InsertMissingModelPricing([]ModelPricing{
		{ModelPattern: "claude-opus-4-6", InputPerMTok: 999.0, OutputPerMTok: 999.0},
		{ModelPattern: "gpt-5.4", InputPerMTok: 2.5, OutputPerMTok: 15.0},
	})
	requireNoError(t, err, "InsertMissingModelPricing")

	// Existing row is untouched.
	opus, err := d.GetModelPricing("claude-opus-4-6")
	requireNoError(t, err, "GetModelPricing opus")
	if opus == nil || opus.InputPerMTok != 5.0 {
		t.Fatalf("opus InputPerMTok = %v, want 5.0 (not overwritten)", opus)
	}
	// New row was inserted.
	gpt, err := d.GetModelPricing("gpt-5.4")
	requireNoError(t, err, "GetModelPricing gpt")
	if gpt == nil || gpt.InputPerMTok != 2.5 {
		t.Fatalf("gpt-5.4 InputPerMTok = %v, want 2.5 (inserted)", gpt)
	}
}
