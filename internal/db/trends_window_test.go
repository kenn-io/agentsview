package db_test

import (
	"testing"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestGetTrendsTermsSQLiteDateWindowEdges(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedTrendsWindow(t, d)
	dbtest.AssertTrendsWindow(t, d)
}
