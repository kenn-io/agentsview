package db_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
)

func TestUsageGroups(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	conn, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	defer conn.Close()
	dbtest.SeedUsageGroups(t, conn)
	dbtest.AssertUsageGroups(t, database)
}

func TestUsageGroupsRemainderUsesRankedOrder(t *testing.T) {
	rows, err := db.GroupTopSessions([]db.TopSessionEntry{
		{SessionID: "low", InputTokens: 3, Cost: money.Money{Microdollars: 2}},
		{SessionID: "high", InputTokens: 20, Cost: money.Money{Microdollars: 30}},
		{SessionID: "middle", InputTokens: 7, Cost: money.Money{Microdollars: 10}},
	}, 1, db.TopSessionsSortCost, db.UsageTokenTypesAll)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "high", rows[0].SessionID)
	assert.Empty(t, rows[1].SessionID)
	assert.Equal(t, 10, rows[1].InputTokens)
	assert.Equal(t, int64(12), rows[1].Cost.Microdollars)
}
