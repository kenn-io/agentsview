package db

import (
	"database/sql"

	"github.com/mattn/go-sqlite3"
)

const sqliteUsageDriverName = "agentsview_sqlite3"

func init() {
	sql.Register(sqliteUsageDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			if err := conn.RegisterFunc(
				"agentsview_usage_output_tokens",
				sqliteUsageOutputTokens,
				true,
			); err != nil {
				return err
			}
			return conn.RegisterFunc(
				"agentsview_usage_web_search_requests",
				parseUsageWebSearchRequests,
				true,
			)
		},
	})
}

func sqliteUsageOutputTokens(tokenJSON string) int {
	_, outputTokens, _, _ := parseUsageTokenCounters(tokenJSON)
	return outputTokens
}
