//go:build fts5

package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Record the executed SQL and bindings without requiring SQLite trace support.
type usagePlanConnector struct {
	base  driver.Driver
	dsn   string
	query string
	args  []any
}

func (c *usagePlanConnector) Driver() driver.Driver { return c.base }

func (c *usagePlanConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &usagePlanConn{SQLiteConn: conn.(*sqlite3.SQLiteConn), recorder: c}, nil
}

type usagePlanConn struct {
	*sqlite3.SQLiteConn
	recorder *usagePlanConnector
}

func (c *usagePlanConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.HasPrefix(query, "SELECT s.session_id, s.project, s.baked_agent, s.machine, s.baked_started_at") {
		c.recorder.query = query
		c.recorder.args = nil
		for _, arg := range args {
			c.recorder.args = append(c.recorder.args, arg.Value)
		}
	}
	return c.SQLiteConn.QueryContext(ctx, query, args)
}

func TestUsageDayTotalsOwnerLookupSeeksDates(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", Project: "project"}
	cache := warmJournaledUsage(t, database, filter)
	original := cache.db
	recorder := &usagePlanConnector{base: original.Driver(), dsn: makeDSN(cache.path, false)}
	cache.db = sql.OpenDB(recorder)
	cache.db.SetMaxOpenConns(1)
	defer func() {
		require.NoError(t, cache.db.Close())
		cache.db = original
	}()
	for _, bounds := range []struct{ from, to string }{
		{"2026-08-10", "2026-08-10"},
		{"2026-08-10", ""},
		{"", "2026-08-10"},
	} {
		t.Run(bounds.from+"/"+bounds.to, func(t *testing.T) {
			filter.From, filter.To = bounds.from, bounds.to
			recorder.query, recorder.args = "", nil
			got, err := database.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, 100, got.Totals.InputTokens)
			conn, err := cache.db.Conn(t.Context())
			require.NoError(t, err)
			defer conn.Close()
			require.NotEmpty(t, recorder.query, "Summary must execute owner discovery")
			rows, err := conn.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+recorder.query, recorder.args...)
			require.NoError(t, err)
			defer rows.Close()
			plan := strings.Join(explainQueryPlanDetails(t, rows), "\n")
			dateSearch := "local_date>"
			if bounds.from == "" {
				dateSearch = "local_date<"
			}
			assert.Contains(t, plan, "SEARCH r USING COVERING INDEX usage_daily_rollups_window ("+dateSearch)
			assert.Contains(t, plan, "SEARCH e USING COVERING INDEX usage_rollup_exceptions_window ("+dateSearch)
		})
	}
}
