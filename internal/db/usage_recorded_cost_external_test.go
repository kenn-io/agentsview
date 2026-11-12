//go:build fts5

package db_test

import (
	"testing"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestRecordedCostPricesOnlyUnpricedModels(t *testing.T) {
	local := dbtest.OpenTestDB(t)
	dbtest.SeedRecordedCostSessions(t, local)
	dbtest.AssertRecordedCostUsage(t, local)
}
