package db_test

import (
	"testing"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestLatestRateLimitsAcceptance(t *testing.T) {
	local := dbtest.OpenTestDB(t)
	dbtest.SeedRateLimitSnapshots(t, local)
	dbtest.AssertLatestRateLimits(t, local)
}
