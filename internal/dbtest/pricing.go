package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// SeedMirrorPricing avoids copying the full fallback pricing table on every push.
func SeedMirrorPricing(t *testing.T, database *db.DB) {
	t.Helper()
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "duckdb-test-seed",
	}}))
}
