package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricing"
)

func TestFmtCost(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{"zero is $0.00", 0, "$0.00"},
		{"under half a cent shows <$0.01", 0.001, "<$0.01"},
		{"half a cent rounds up to $0.01", 0.005, "$0.01"},
		{"typical cents", 0.45, "$0.45"},
		{"dollars", 12.34, "$12.34"},
		{"rounds to two decimals", 1.23456, "$1.23"},
		{"large value", 1234.56, "$1234.56"},
		// A negative input shouldn't hit the <$0.01 branch.
		{"negative passes through", -0.42, "$-0.42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, fmtCost(tc.in),
				"fmtCost(%v)", tc.in)
		})
	}
}

func TestResolveDefaultSince(t *testing.T) {
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	const utc = "UTC"

	tests := []struct {
		name  string
		since string
		until string
		all   bool
		want  string
	}{
		{
			name: "no flags returns 30-day window",
			want: "2024-05-17",
		},
		{
			name:  "explicit since preserved",
			since: "2024-01-01",
			want:  "2024-01-01",
		},
		{
			name: "all flag disables default",
			all:  true,
			want: "",
		},
		{
			name:  "until without since does not backfill since",
			until: "2024-01-31",
			want:  "",
		},
		{
			name:  "explicit range preserved",
			since: "2024-01-01",
			until: "2024-01-31",
			want:  "2024-01-01",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDefaultSince(
				tc.since, tc.until, tc.all, now, utc,
			)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestFormatDailyUsageJSON(t *testing.T) {
	result := db.DailyUsageResult{
		Daily: []db.DailyUsageEntry{
			{
				Date:                "2024-06-15",
				InputTokens:         50000,
				OutputTokens:        12000,
				CacheCreationTokens: 8000,
				CacheReadTokens:     30000,
				TotalCost:           0.45,
				ModelsUsed:          []string{"claude-sonnet-4-20250514"},
				ModelBreakdowns: []db.ModelBreakdown{
					{
						ModelName:           "claude-sonnet-4-20250514",
						InputTokens:         50000,
						OutputTokens:        12000,
						CacheCreationTokens: 8000,
						CacheReadTokens:     30000,
						Cost:                0.45,
					},
				},
			},
		},
		Totals: db.UsageTotals{
			InputTokens:         50000,
			OutputTokens:        12000,
			CacheCreationTokens: 8000,
			CacheReadTokens:     30000,
			TotalCost:           0.45,
		},
	}

	out, err := json.Marshal(result)
	require.NoError(t, err, "json.Marshal failed")

	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &decoded),
		"json.Unmarshal failed")

	assert.Contains(t, decoded, "daily", "missing 'daily' key in JSON output")
	assert.Contains(t, decoded, "totals", "missing 'totals' key in JSON output")

	// Verify daily array has expected entry
	var daily []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(decoded["daily"], &daily),
		"parsing daily array")
	require.Len(t, daily, 1, "daily length")

	// Check expected fields exist in daily entry
	wantFields := []string{
		"date", "inputTokens", "outputTokens",
		"cacheCreationTokens", "cacheReadTokens",
		"totalCost", "modelsUsed", "modelBreakdowns",
	}
	for _, f := range wantFields {
		assert.Contains(t, daily[0], f,
			"missing field %q in daily entry", f)
	}

	// Verify totals fields
	var totals map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(decoded["totals"], &totals),
		"parsing totals")
	totalFields := []string{
		"inputTokens", "outputTokens",
		"cacheCreationTokens", "cacheReadTokens",
		"totalCost",
	}
	for _, f := range totalFields {
		assert.Contains(t, totals, f,
			"missing field %q in totals", f)
	}
}

func TestRefreshPricingIfStale_FreshAttemptSkipsFetch(t *testing.T) {
	d := newTestDB(t)
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)

	// Last attempt 10 minutes ago, cooldown 1 hour: skip.
	prev := now.Add(-10 * time.Minute).Format(time.RFC3339)
	if err := d.SetPricingMeta(
		pricingRefreshMetaKey, prev,
	); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	called := false
	refreshed, err := refreshPricingIfStale(
		d, func() ([]pricing.ModelPricing, error) {
			called = true
			return nil, nil
		}, time.Hour, now,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if refreshed {
		t.Error("refreshed = true, want false within cooldown")
	}
	if called {
		t.Error("fetch should not run within cooldown")
	}

	// Meta value preserved (we did not overwrite it).
	got, err := d.GetPricingMeta(pricingRefreshMetaKey)
	if err != nil {
		t.Fatalf("get meta: %v", err)
	}
	if got != prev {
		t.Errorf("meta = %q, want %q (unchanged)", got, prev)
	}
}

func TestRefreshPricingIfStale_StaleTriggersFetch(t *testing.T) {
	d := newTestDB(t)
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)

	// Last attempt 2 hours ago, cooldown 1 hour: refresh.
	prev := now.Add(-2 * time.Hour).Format(time.RFC3339)
	if err := d.SetPricingMeta(
		pricingRefreshMetaKey, prev,
	); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	refreshed, err := refreshPricingIfStale(
		d, func() ([]pricing.ModelPricing, error) {
			return []pricing.ModelPricing{{
				ModelPattern:  "gpt-5.5",
				InputPerMTok:  1.25,
				OutputPerMTok: 10.0,
			}}, nil
		}, time.Hour, now,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !refreshed {
		t.Fatal("refreshed = false, want true after cooldown")
	}

	// Pricing row written.
	p, err := d.GetModelPricing("gpt-5.5")
	if err != nil {
		t.Fatalf("get pricing: %v", err)
	}
	if p == nil || p.OutputPerMTok != 10.0 {
		t.Errorf("gpt-5.5 row missing or wrong: %+v", p)
	}

	// Meta updated to now.
	got, err := d.GetPricingMeta(pricingRefreshMetaKey)
	if err != nil {
		t.Fatalf("get meta: %v", err)
	}
	if got != now.Format(time.RFC3339) {
		t.Errorf("meta = %q, want %q", got, now.Format(time.RFC3339))
	}
}

func TestRefreshPricingIfStale_NeverAttemptedTriggersFetch(t *testing.T) {
	d := newTestDB(t)
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)

	called := false
	refreshed, err := refreshPricingIfStale(
		d, func() ([]pricing.ModelPricing, error) {
			called = true
			return nil, nil
		}, time.Hour, now,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !called {
		t.Error("fetch should run when meta empty")
	}
	if !refreshed {
		t.Error("refreshed = false, want true on first attempt")
	}
}

func TestRefreshPricingIfStale_FetchFailureRecordsAttempt(t *testing.T) {
	d := newTestDB(t)
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)

	wantErr := errors.New("network down")
	refreshed, err := refreshPricingIfStale(
		d, func() ([]pricing.ModelPricing, error) {
			return nil, wantErr
		}, time.Hour, now,
	)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want network down", err)
	}
	if refreshed {
		t.Error("refreshed = true, want false on fetch failure")
	}

	// Cooldown still recorded so a persistent failure doesn't
	// retry on every CLI call.
	got, err := d.GetPricingMeta(pricingRefreshMetaKey)
	if err != nil {
		t.Fatalf("get meta: %v", err)
	}
	if got != now.Format(time.RFC3339) {
		t.Errorf("meta = %q, want %q (recorded on failure)",
			got, now.Format(time.RFC3339))
	}

	// A second call within cooldown skips the fetch entirely.
	called := false
	_, err = refreshPricingIfStale(
		d, func() ([]pricing.ModelPricing, error) {
			called = true
			return nil, nil
		}, time.Hour, now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if called {
		t.Error("second call should be suppressed by cooldown")
	}
}

func TestEnsurePricingWithFetcherSkipsFetchWithinCooldown(t *testing.T) {
	d := newTestDB(t)
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)

	prev := now.Add(-10 * time.Minute).Format(time.RFC3339)
	require.NoError(t, d.SetPricingMeta(pricingRefreshMetaKey, prev))

	called := false
	refreshed, err := ensurePricingWithFetcher(
		d, false, func() ([]pricing.ModelPricing, error) {
			called = true
			return []pricing.ModelPricing{{
				ModelPattern:  "network-only-model",
				InputPerMTok:  1,
				OutputPerMTok: 1,
			}}, nil
		}, now,
	)
	require.NoError(t, err)
	assert.False(t, refreshed)
	assert.False(t, called, "fetch should not run within cooldown")

	fallback, err := d.GetModelPricing("gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, fallback, "fallback pricing should be seeded")

	networkOnly, err := d.GetModelPricing("network-only-model")
	require.NoError(t, err)
	assert.Nil(t, networkOnly, "cooldown should prevent network upsert")
}
