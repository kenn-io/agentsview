//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
)

// A mirror that kept the OpenRouter ownership list as a model_pricing row
// gets the list moved to pricing_meta at startup, and the row removed, so
// the newest pricing time comes from a real price.
func TestEnsureSchemaMovesPricingMetaOutOfModelPricing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	require.NoError(t, EnsureSchemaOn(ctx, conn))

	_, err := conn.ExecContext(ctx, `INSERT INTO model_pricing
		(model_pattern, updated_at, push_version) VALUES
		('_openrouter_models', '["or/model-a"]', 1),
		('acme/model', '2026-07-01T00:00:02.000Z', 1)`)
	require.NoError(t, err)

	require.NoError(t, EnsureSchemaOn(ctx, conn))

	owned, err := readPricingMeta(ctx, conn, "_openrouter_models")
	require.NoError(t, err)
	assert.Equal(t, `["or/model-a"]`, owned)
	prices, err := readModelPricing(ctx, conn)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	assert.Equal(t, "acme/model", prices[0].ModelPattern)
}
