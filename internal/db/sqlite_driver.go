package db

import (
	"database/sql"
	"time"

	"github.com/mattn/go-sqlite3"
)

const sqliteUsageDriverName = "agentsview_sqlite3"

func init() {
	sql.Register(sqliteUsageDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			if err := conn.RegisterFunc(
				"agentsview_timestamp_unix_micro",
				sqliteTimestampUnixMicro,
				true,
			); err != nil {
				return err
			}
			if err := conn.RegisterFunc(
				"agentsview_local_timestamp", sqliteLocalTimestamp, true,
			); err != nil {
				return err
			}
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

func sqliteTimestampUnixMicro(raw string) any {
	timestamp, ok := ParseStoredTimestamp(raw)
	if !ok {
		return nil
	}
	return timestamp.UTC().UnixMicro()
}

func sqliteLocalTimestamp(value any, timezone string) any {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return nil
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil
	}
	return parsed.In(location).Format("2006-01-02 15:04:05.999999999")
}

func sqliteUsageOutputTokens(tokenJSON string) int {
	_, outputTokens, _, _ := parseUsageTokenCounters(tokenJSON)
	return outputTokens
}
