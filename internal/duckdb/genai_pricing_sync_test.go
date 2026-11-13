//go:build duckdb && !(windows && arm64)

package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

// The push compares the mirror's GenAI document by a digest DuckDB computes.
// A digest that disagreed with the archive's would rewrite the document, and
// its observation time, on every push.
func TestSyncGenAIPricingComparesByDigest(t *testing.T) {
	for name, seed := range map[string]*db.GenAIPricingDocument{
		"embedded fallback": nil,
		"fetched": {
			Version: "genai-prices-test", SourceRef: "upstream-ref",
			Source: db.GenAIPricingSourceFetched, Data: []byte(`{"test":true}`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			local := dbtest.OpenTestDB(t)
			if seed != nil {
				require.NoError(t, local.UpsertGenAIPricing(ctx, *seed))
			}
			syncer := newInMemoryTestSync(t, local, storage.MirrorPushOptions{})
			require.NoError(t, createSchema(ctx, syncer.DB()))
			mirrored := func() (data []byte, updatedAt string) {
				t.Helper()
				require.NoError(t, syncer.DB().QueryRowContext(ctx,
					`SELECT data_json, updated_at FROM genai_pricing WHERE singleton = 1`,
				).Scan(&data, &updatedAt))
				return data, updatedAt
			}

			require.NoError(t, syncer.syncGenAIPricing(ctx))
			data, _ := mirrored()
			want := db.EmbeddedGenAIPricingDocument().Data
			if seed != nil {
				want = seed.Data
			}
			require.Equal(t, want, data, "a missing mirror document is inserted")

			_, err := syncer.DB().ExecContext(ctx,
				`UPDATE genai_pricing SET updated_at = 'mirror-observed' WHERE singleton = 1`)
			require.NoError(t, err)
			require.NoError(t, syncer.syncGenAIPricing(ctx))
			_, updatedAt := mirrored()
			assert.Equal(t, "mirror-observed", updatedAt, "a matching document is not rewritten")

			require.NoError(t, local.UpsertGenAIPricing(ctx, db.GenAIPricingDocument{
				Version: "genai-prices-test", SourceRef: "upstream-ref",
				Source: db.GenAIPricingSourceFetched, Data: []byte(`{"test":"changed"}`),
			}))
			require.NoError(t, syncer.syncGenAIPricing(ctx))
			data, updatedAt = mirrored()
			assert.JSONEq(t, `{"test":"changed"}`, string(data), "changed data is pushed")
			assert.NotEqual(t, "mirror-observed", updatedAt)
		})
	}
}
