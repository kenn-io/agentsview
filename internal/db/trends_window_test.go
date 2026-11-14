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
	dbtest.AssertTrendsPartsReuse(t, func(retained bool) db.Store {
		open := db.OpenReadOnly
		if retained {
			open = db.Open
		}
		opened, err := open(t.Context(), d.Path())
		require.NoError(t, err)
		return opened
	}, func(action string) { dbtest.ChangeTrendsWindow(t, d, action) })
}
