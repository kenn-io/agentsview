package db_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestUsageGroups(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	conn, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	defer conn.Close()
	dbtest.SeedUsageGroups(t, conn)
	dbtest.AssertUsageGroups(t, database)
}
