package db_test

import (
	"testing"

	"go.kenn.io/agentsview/internal/dbtest"
)

// TestSQLiteSessionUsagePins pins the archive's session usage responses for
// sessions with and without usage, including one that gains usage after its
// first read.
func TestSQLiteSessionUsagePins(t *testing.T) {
	local := dbtest.OpenTestDB(t)
	dbtest.SeedSessionUsagePins(t, local)
	dbtest.AssertSessionUsagePins(t, local)
	dbtest.GrowSessionUsagePin(t, local)
	dbtest.AssertGrownSessionUsagePin(t, local)
}
