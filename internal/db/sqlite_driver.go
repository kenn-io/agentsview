package db

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"

	"github.com/mattn/go-sqlite3"
)

const (
	sqliteUsageDriverName   = "agentsview_sqlite3"
	sqliteArchiveDriverName = "agentsview_archive_sqlite3"
)

// simpleFTSJiebaMu serializes the two upstream entry points that share
// cppjieba's module-global dictionary state: jieba_dict and jieba_query.
// The simple FTS tokenizer used by MATCH, trigger maintenance, and rebuilds
// does not read that state; it tokenizes documents directly by code point.
var simpleFTSJiebaMu sync.Mutex

func init() {
	sql.Register(sqliteUsageDriverName, &sqlite3.SQLiteDriver{
		ConnectHook: configureSQLiteConnection,
	})
	drv := &sqlite3.SQLiteDriver{ConnectHook: configureArchiveSQLiteConnection}
	if simpleFTSRuntimeConfig.available() {
		drv.Extensions = []string{simpleFTSRuntimeConfig.libraryPath}
	}
	sql.Register(sqliteArchiveDriverName, drv)
}

func configureSQLiteConnection(conn *sqlite3.SQLiteConn) error {
	if err := conn.RegisterFunc(
		"agentsview_timestamp_unix_micro",
		sqliteTimestampUnixMicro,
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
	if err := conn.RegisterFunc(
		"agentsview_usage_web_search_requests",
		parseUsageWebSearchRequests,
		true,
	); err != nil {
		return err
	}
	return nil
}

func configureArchiveSQLiteConnection(conn *sqlite3.SQLiteConn) error {
	if err := configureSQLiteConnection(conn); err != nil {
		return err
	}
	if err := conn.RegisterFunc(
		"agentsview_chinese_fts_fingerprint",
		func() string { return simpleFTSRuntimeConfig.fingerprint },
		true,
	); err != nil {
		return fmt.Errorf("registering Chinese FTS fingerprint: %w", err)
	}
	if !simpleFTSRuntimeConfig.available() {
		return nil
	}

	// Upstream reads jieba_dict_path only while constructing/using
	// jieba_query's process-global cppjieba instance. Serialize the setter
	// with every internal jieba_query call. MATCH and index maintenance use
	// SimpleTokenizer::tokenize and do not consult this path.
	simpleFTSJiebaMu.Lock()
	defer simpleFTSJiebaMu.Unlock()
	if _, err := conn.Exec(
		"SELECT jieba_dict(?)",
		[]driver.Value{simpleFTSRuntimeConfig.dictionaryPath},
	); err != nil {
		return fmt.Errorf("configuring simple FTS5 dictionaries: %w", err)
	}
	return nil
}

func sqliteTimestampUnixMicro(raw string) any {
	timestamp, ok := ParseStoredTimestamp(raw)
	if !ok {
		return nil
	}
	return timestamp.UTC().UnixMicro()
}

func sqliteUsageOutputTokens(tokenJSON string) int {
	_, outputTokens, _, _ := parseUsageTokenCounters(tokenJSON)
	return outputTokens
}
