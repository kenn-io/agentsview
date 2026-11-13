//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
)

// A mirror in the old pricing layout kept updated_at as a string and the
// OpenRouter ownership list as a model_pricing row. Startup moves the list
// to pricing_meta, removes the row, and converts updated_at to a nullable
// timestamp, with an empty time becoming NULL. model_pricing_bands is left
// halfway through the conversion, as a stopped run leaves it, and the
// rerun finishes it.
func TestEnsureSchemaUpgradesOldPricingLayout(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dsn, database := chtest.FreshDatabase(t)
	conn := chtest.Open(t, dsn, database)
	require.NoError(t, EnsureSchemaOn(ctx, conn))

	for _, statement := range []string{
		`ALTER TABLE model_pricing DROP COLUMN updated_at`,
		`ALTER TABLE model_pricing ADD COLUMN updated_at String`,
		`ALTER TABLE genai_pricing DROP COLUMN updated_at`,
		`ALTER TABLE genai_pricing ADD COLUMN updated_at String`,
		`ALTER TABLE model_pricing_bands DROP COLUMN updated_at`,
		`ALTER TABLE model_pricing_bands ADD COLUMN updated_at_text String`,
		`INSERT INTO model_pricing (model_pattern, updated_at, push_version) VALUES
			('_openrouter_models', '["or/model-a"]', 1),
			('acme/model', '2026-07-01T00:00:02.250Z', 1)`,
		`INSERT INTO model_pricing_bands
			(model_pattern, above_input_tokens, updated_at_text, push_version)
		 VALUES ('acme/model', 1000, '2026-07-01T00:00:03Z', 1)`,
		`INSERT INTO genai_pricing (singleton, version, source, data_json, updated_at, push_version)
		 VALUES (1, 'v1', 'embedded', '{}', '', 1)`,
	} {
		_, err := conn.ExecContext(ctx, statement)
		require.NoError(t, err, statement)
	}
	require.ErrorContains(t, CheckSchemaCompat(ctx, conn), "updated_at as text",
		"a reader must not scan text times before the upgrade")

	require.NoError(t, EnsureSchemaOn(ctx, conn))

	owned, err := readPricingMeta(ctx, conn, "_openrouter_models")
	require.NoError(t, err)
	assert.Equal(t, `["or/model-a"]`, owned)
	prices, err := readModelPricing(ctx, conn)
	require.NoError(t, err)
	require.Len(t, prices, 1, "only the real price remains")
	assert.Equal(t, "acme/model", prices[0].ModelPattern)
	assert.Equal(t, "2026-07-01T00:00:02.25Z", prices[0].UpdatedAt)
	require.Len(t, prices[0].Bands, 1)
	assert.Equal(t, "2026-07-01T00:00:03Z", prices[0].Bands[0].UpdatedAt)
	document, err := loadGenAIPricing(ctx, conn)
	require.NoError(t, err)
	require.NotNil(t, document)
	assert.Empty(t, document.UpdatedAt, "an empty time becomes NULL")

	columns, err := readColumns(ctx, conn)
	require.NoError(t, err)
	for _, table := range []string{"model_pricing", "model_pricing_bands", "genai_pricing"} {
		assert.Equal(t, tNullTime, columns[table]["updated_at"], table)
		assert.NotContains(t, columns[table], "updated_at_text", table)
	}
}
