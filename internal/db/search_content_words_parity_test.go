package db_test

import (
	"testing"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestSearchContentWordParity(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedWordSearch(t, d)
	dbtest.AssertWordSearch(t, d)
}
