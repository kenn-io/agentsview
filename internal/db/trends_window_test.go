package db_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestGetTrendsTermsSQLiteDateWindowEdges(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedTrendsWindow(t, d)
	dbtest.AssertTrendsWindow(t, d)
	dbtest.AssertTrendsPartsReuse(t, func() db.Store {
		opened, err := db.Open(t.Context(), d.Path())
		require.NoError(t, err)
		return opened
	}, func() { dbtest.RewriteTrendsWindow(t, d) })
}
