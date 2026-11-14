package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

// Opening an archive that kept pricing metadata as model_pricing rows moves
// each value to pricing_meta and leaves only model prices behind.
func TestOpenMovesPricingMetaOutOfModelPricing(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "archive.db")
	d, err := OpenIsolated(ctx, path)
	require.NoError(t, err)
	require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
		ModelPattern: "acme/model", InputPerMTok: money.MustParseDollars("2"),
	}}))
	for _, statement := range []string{
		"DROP TABLE pricing_meta",
		`INSERT INTO model_pricing (model_pattern, updated_at)
		 VALUES ('_openrouter_models', '["or/model-a"]'),
		        ('_fallback_version', 'seed-7')`,
	} {
		_, err := d.getWriter().Exec(ctx, statement)
		require.NoError(t, err, statement)
	}
	require.NoError(t, d.Close())

	upgraded, err := OpenIsolated(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })

	for key, want := range map[string]string{
		"_openrouter_models": `["or/model-a"]`,
		"_fallback_version":  "seed-7",
	} {
		got, err := upgraded.GetPricingMeta(key)
		require.NoError(t, err, key)
		assert.Equal(t, want, got, key)
	}
	prices, err := upgraded.ListModelPricing(ctx)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	assert.Equal(t, "acme/model", prices[0].ModelPattern)
}
