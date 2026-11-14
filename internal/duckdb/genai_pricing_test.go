//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/pricing"
)

// The pricing row follows the stored document's bytes: replacing the
// document must not keep serving the parse of the old one.
func TestLoadDuckGenAIPricingRowFollowsStoredDocument(t *testing.T) {
	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(t, createSchema(ctx, conn))
	queryRow := func(ctx context.Context, query string, args ...any) interface{ Scan(...any) error } {
		return conn.QueryRowContext(ctx, query, args...)
	}

	row, err := loadDuckGenAIPricingRow(ctx, queryRow)
	require.NoError(t, err)
	assert.Equal(t, export.PricingRowSourceEmbedded, row.GenAISource,
		"an empty table prices with the embedded document")

	// Both documents carry an empty version, so only their bytes tell them
	// apart.
	embedded := pricing.EmbeddedGenAIDocument()
	_, err = conn.ExecContext(ctx, `
		INSERT INTO genai_pricing (singleton, version, source_ref, source, data_json, updated_at)
		VALUES (1, '', 'ref-a', 'fetched', ?, NULL)`,
		embedded.RawJSON())
	require.NoError(t, err)
	row, err = loadDuckGenAIPricingRow(ctx, queryRow)
	require.NoError(t, err)
	assert.Equal(t, export.PricingRowSourceFetched, row.GenAISource)
	assert.Equal(t, embedded.Version, row.GenAIVersion)

	changed := append(slices.Clone(embedded.RawJSON()), '\n')
	_, err = conn.ExecContext(ctx,
		`UPDATE genai_pricing SET data_json = ? WHERE singleton = 1`, changed)
	require.NoError(t, err)
	row, err = loadDuckGenAIPricingRow(ctx, queryRow)
	require.NoError(t, err)
	assert.NotEqual(t, embedded.Version, row.GenAIVersion,
		"a replaced document must be parsed again")
}
