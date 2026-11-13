//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A mirror that kept the OpenRouter ownership list as a model_pricing row
// gets the list moved to pricing_meta by EnsureSchema, and the row removed,
// so the newest pricing time comes from a real price.
func TestEnsureSchemaMovesPricingMetaOutOfModelPricing(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_pricing_meta_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(t.Context(), pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	ctx := t.Context()
	require.NoError(t, EnsureSchema(ctx, pg, schema))

	for _, statement := range []string{
		`DROP TABLE pricing_meta`,
		`INSERT INTO model_pricing (model_pattern, updated_at) VALUES
			('_openrouter_models', '["or/model-a"]'),
			('acme/model', '2026-07-01T00:00:02.000Z')`,
	} {
		_, err := pg.ExecContext(ctx, statement)
		require.NoError(t, err, statement)
	}

	require.NoError(t, EnsureSchema(ctx, pg, schema))

	var owned string
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT value FROM pricing_meta WHERE key = '_openrouter_models'`,
	).Scan(&owned))
	assert.Equal(t, `["or/model-a"]`, owned)
	var patterns []string
	rows, err := pg.QueryContext(ctx, `SELECT model_pattern FROM model_pricing ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var pattern string
		require.NoError(t, rows.Scan(&pattern))
		patterns = append(patterns, pattern)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"acme/model"}, patterns)
}
