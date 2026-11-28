package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/money"
)

// Usage snapshots reuse the loaded pricing catalog between refreshes. Every
// change to the stored catalog or to the in-memory pricing inputs must still
// reach the next snapshot.
func TestUsageSnapshotPricingFollowsPricingChanges(t *testing.T) {
	d := testDB(t)
	capture := func() usageQuerySnapshot {
		t.Helper()
		snapshot, err := d.captureUsageSessions(t.Context(), []string{"no-such-session"})
		require.NoError(t, err)
		return snapshot
	}
	outputRate := func(snapshot usageQuerySnapshot, pattern string) money.Money {
		t.Helper()
		for _, row := range snapshot.PricingRows {
			if row.ModelPattern == pattern {
				return row.Rates.OutputPerMTok
			}
		}
		require.Failf(t, "pricing row missing", "pattern %q", pattern)
		return money.Money{}
	}

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "cached-model", OutputPerMTok: money.MustParseDollars("10"),
	}}))
	first := capture()
	assert.Equal(t, money.MustParseDollars("10"), outputRate(first, "cached-model"))

	again := capture()
	assert.Equal(t, first.PricingDigest, again.PricingDigest)
	require.NotEmpty(t, again.PricingRows)
	assert.Same(t, &first.PricingRows[0], &again.PricingRows[0],
		"an unchanged catalog should be reused, not reloaded")

	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "cached-model", OutputPerMTok: money.MustParseDollars("15"),
	}}))
	updated := capture()
	assert.Equal(t, money.MustParseDollars("15"), outputRate(updated, "cached-model"),
		"a stored catalog change must reach the next snapshot")
	assert.NotEqual(t, first.PricingDigest, updated.PricingDigest)

	d.SetCustomPricing(map[string]config.CustomModelRate{
		"custom-model": {OutputMicrodollarsPerMTok: 20_000_000},
	})
	custom := capture()
	assert.Equal(t, money.MustParseDollars("20"), outputRate(custom, "custom-model"),
		"an in-memory pricing change must reach the next snapshot")
	assert.NotEqual(t, updated.PricingDigest, custom.PricingDigest)
}
