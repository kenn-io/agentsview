package parser

import (
	"database/sql"
	"strconv"
	"strings"
)

type sqliteReadOptions struct {
	// Only archive copies that cannot change may skip SQLite locking and WAL reads.
	stableSnapshot bool
	// Zero preserves the driver's busy timeout rather than overriding it.
	busyTimeoutMS int
}

// openSQLiteReadOnly shares source-database URI and read policies. Opening is
// lazy, as with sql.Open; callers retain ownership of queries and Close.
func openSQLiteReadOnly(path string, options sqliteReadOptions) (*sql.DB, error) {
	dsn := "file:" + sqliteURIPath(path) + "?mode=ro"
	if options.stableSnapshot {
		dsn += "&immutable=1"
	}
	if options.busyTimeoutMS != 0 {
		dsn += "&_busy_timeout=" + strconv.Itoa(options.busyTimeoutMS)
	}
	return sql.Open("sqlite3", dsn)
}

// sqliteURIPath escapes a filesystem path for use in a SQLite file: URI.
// SQLite percent-decodes URI paths and stops parsing them at '?' or '#',
// and go-sqlite3 splits its driver parameters at the first '?', so these
// characters in a real path would open the wrong file or silently drop
// options such as mode=ro.
func sqliteURIPath(path string) string {
	return strings.NewReplacer(
		"%", "%25",
		"?", "%3F",
		"#", "%23",
	).Replace(path)
}
