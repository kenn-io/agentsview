//go:build pgtest

package postgres

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/pricing"
)

// The pricing row follows the stored document's bytes on PostgreSQL, whose
// sha256 returns raw bytes, and on CockroachDB, whose sha256 returns hex.
func TestLoadPGGenAIPricingRowFollowsStoredDocument(t *testing.T) {
	ctx := t.Context()
	pgURL := testPGURL(t)
	const schema = "agentsview_genai_digest_test"
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(ctx, pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(ctx, pg, schema))

	row, err := loadPGGenAIPricingRow(ctx, pg, true)
	require.NoError(t, err)
	assert.Equal(t, export.PricingRowSourceEmbedded, row.GenAISource,
		"an empty table prices with the embedded document")

	// Both documents carry an empty version, so only their bytes tell them
	// apart.
	embedded := pricing.EmbeddedGenAIDocument()
	_, err = pg.ExecContext(ctx, `
		INSERT INTO genai_pricing (singleton, version, source_ref, source, data_json, updated_at)
		VALUES (1, '', 'ref-a', 'fetched', $1, '')`, embedded.RawJSON())
	require.NoError(t, err)
	row, err = loadPGGenAIPricingRow(ctx, pg, true)
	require.NoError(t, err)
	assert.Equal(t, export.PricingRowSourceFetched, row.GenAISource)
	assert.Equal(t, embedded.Version, row.GenAIVersion)

	changed := append(slices.Clone(embedded.RawJSON()), '\n')
	_, err = pg.ExecContext(ctx,
		`UPDATE genai_pricing SET data_json = $1 WHERE singleton = 1`, changed)
	require.NoError(t, err)
	row, err = loadPGGenAIPricingRow(ctx, pg, true)
	require.NoError(t, err)
	assert.NotEqual(t, embedded.Version, row.GenAIVersion,
		"a replaced document must be parsed again")
}
