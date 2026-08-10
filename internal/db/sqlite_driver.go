package db

import (
	"database/sql"

	"github.com/mattn/go-sqlite3"
	"go.kenn.io/agentsview/internal/db/bunmodel"
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
				"agentsview_canonical_timestamp",
				sqliteCanonicalTimestamp,
				true,
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

func sqliteCanonicalTimestamp(value any) any {
	var text string
	switch value := value.(type) {
	case string:
		text = value
	case []byte:
		if value == nil {
			return nil
		}
		text = string(value)
	default:
		return nil
	}
	timestamp, err := bunmodel.ParseTimestamp(text)
	if err != nil || timestamp.IsZero() {
		return nil
	}
	return text
}

func sqliteUsageOutputTokens(tokenJSON string) int {
	_, outputTokens, _, _ := parseUsageTokenCounters(tokenJSON)
	return outputTokens
}
