package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// A metadata row skips the fallback pricing copy without giving PostgreSQL a usable price.
func SeedPricingMeta(t *testing.T, d *db.DB) {
	t.Helper()
	require.NoError(t, d.SetPricingMeta("_test_seed", "1"))
}
