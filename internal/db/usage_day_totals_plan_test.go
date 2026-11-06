//go:build fts5 && sqlite_trace

package db

import (
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageDayTotalsOwnerLookupSeeksDates(t *testing.T) {
	database := testDB(t)
	seedJournaledUsage(t, database, "session")
	filter := UsageFilter{From: "2026-08-10", To: "2026-08-10", Timezone: "UTC", Project: "project"}
	cache := warmJournaledUsage(t, database, filter)
	cache.db.SetMaxOpenConns(1)
	for _, bounds := range []struct{ from, to string }{
		{"2026-08-10", "2026-08-10"},
		{"2026-08-10", ""},
		{"", "2026-08-10"},
	} {
		t.Run(bounds.from+"/"+bounds.to, func(t *testing.T) {
			filter.From, filter.To = bounds.from, bounds.to
			conn, err := cache.db.Conn(t.Context())
			require.NoError(t, err)
			var query string
			require.NoError(t, conn.Raw(func(raw any) error {
				return raw.(*sqlite3.SQLiteConn).SetTrace(&sqlite3.TraceConfig{
					EventMask: sqlite3.TraceStmt, WantExpandedSQL: true,
					Callback: func(info sqlite3.TraceInfo) int {
						if strings.Contains(info.StmtOrTrigger, "SELECT s.session_id, s.project, s.baked_agent, s.machine, s.baked_started_at") {
							query = info.ExpandedSQL
						}
						return 0
					},
				})
			}))
			require.NoError(t, conn.Close())
			got, err := database.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, 100, got.Totals.InputTokens)
			conn, err = cache.db.Conn(t.Context())
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.Raw(func(raw any) error {
				return raw.(*sqlite3.SQLiteConn).SetTrace(nil)
			}))
			require.NotEmpty(t, query, "Summary must execute owner discovery")
			rows, err := conn.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query)
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
