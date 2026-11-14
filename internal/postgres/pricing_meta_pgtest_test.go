//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A store in the old pricing layout kept updated_at as text and the
// OpenRouter ownership list as a model_pricing row. EnsureSchema moves the
// list to pricing_meta, removes the row, and converts updated_at to
// timestamptz, with an empty time becoming NULL. model_pricing_bands is
// left halfway through the conversion, as a stopped run leaves it, and the
// rerun finishes it.
func TestEnsureSchemaUpgradesOldPricingLayout(t *testing.T) {
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
		`ALTER TABLE model_pricing DROP COLUMN updated_at`,
		`ALTER TABLE model_pricing ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE genai_pricing DROP COLUMN updated_at`,
		`ALTER TABLE genai_pricing ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE model_pricing_bands DROP COLUMN updated_at`,
		`ALTER TABLE model_pricing_bands ADD COLUMN updated_at_typed TIMESTAMPTZ`,
		`INSERT INTO model_pricing (model_pattern, updated_at) VALUES
			('_openrouter_models', '["or/model-a"]'),
			('acme/model', '2026-07-01T00:00:02.250Z')`,
		`INSERT INTO model_pricing_bands (model_pattern, above_input_tokens,
			input_microdollars_per_mtok, output_microdollars_per_mtok,
			cache_creation_microdollars_per_mtok, cache_read_microdollars_per_mtok,
			updated_at_typed)
		 VALUES ('acme/model', 1000, 1, 1, 1, 1, '2026-07-01T00:00:03Z')`,
		`INSERT INTO genai_pricing (singleton, version, source, data_json)
		 VALUES (1, 'v1', 'embedded', '\x7b7d')`,
	} {
		_, err := pg.ExecContext(ctx, statement)
		require.NoError(t, err, statement)
	}
	require.ErrorContains(t, CheckSchemaCompat(ctx, pg), "older agentsview version",
		"a reader must not scan text times before the upgrade")

	require.NoError(t, EnsureSchema(ctx, pg, schema))

	var owned string
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT value FROM pricing_meta WHERE key = '_openrouter_models'`,
	).Scan(&owned))
	assert.Equal(t, `["or/model-a"]`, owned)

	var pattern string
	var priceTime, bandTime, genAITime sql.NullTime
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT p.model_pattern, p.updated_at, b.updated_at,
			(SELECT updated_at FROM genai_pricing)
		FROM model_pricing p
		JOIN model_pricing_bands b ON b.model_pattern = p.model_pattern`,
	).Scan(&pattern, &priceTime, &bandTime, &genAITime))
	assert.Equal(t, "acme/model", pattern, "only the real price remains")
	assert.True(t, priceTime.Time.Equal(
		time.Date(2026, 7, 1, 0, 0, 2, 250_000_000, time.UTC)), priceTime)
	assert.True(t, bandTime.Time.Equal(
		time.Date(2026, 7, 1, 0, 0, 3, 0, time.UTC)), bandTime)
	assert.False(t, genAITime.Valid, "an empty time becomes NULL")

	types, err := pricingTimeColumnTypesPG(ctx, pg)
	require.NoError(t, err)
	want := map[string]string{"updated_at": "timestamp with time zone"}
	assert.Equal(t, map[string]map[string]string{
		"model_pricing":       want,
		"model_pricing_bands": want,
		"genai_pricing":       want,
	}, types, "updated_at_typed is gone")
	require.NoError(t, CheckSchemaCompat(ctx, pg))
}

// A start that dropped model_pricing's text updated_at but stopped before
// renaming updated_at_typed must not block the next start, which moves
// pricing metadata by reading updated_at.
func TestEnsureSchemaFinishesInterruptedPricingTimeRename(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_pricing_rename_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(t.Context(), pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	ctx := t.Context()
	require.NoError(t, EnsureSchema(ctx, pg, schema))

	for _, statement := range []string{
		`ALTER TABLE model_pricing DROP COLUMN updated_at`,
		`ALTER TABLE model_pricing ADD COLUMN updated_at_typed TIMESTAMPTZ`,
		`INSERT INTO model_pricing (model_pattern, updated_at_typed)
		 VALUES ('acme/model', '2026-07-01T00:00:02Z')`,
	} {
		_, err := pg.ExecContext(ctx, statement)
		require.NoError(t, err, statement)
	}

	require.NoError(t, EnsureSchema(ctx, pg, schema))

	var priceTime sql.NullTime
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT updated_at FROM model_pricing WHERE model_pattern = 'acme/model'`,
	).Scan(&priceTime))
	assert.True(t, priceTime.Time.Equal(
		time.Date(2026, 7, 1, 0, 0, 2, 0, time.UTC)), priceTime)
	require.NoError(t, CheckSchemaCompat(ctx, pg))
}
