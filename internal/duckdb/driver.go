//go:build !(windows && arm64)

package duckdb

import (
	"database/sql"
	"errors"

	// Registers the "duckdb" database/sql driver, which statically links
	// the prebuilt DuckDB library from duckdb-go-bindings.
	duckdbdriver "github.com/duckdb/duckdb-go/v2"
)

func openDuckDB(dsn string) (*sql.DB, error) {
	return sql.Open("duckdb", dsn)
}

func isMirrorOpenInSameProcessError(err error) bool {
	return errors.Is(err, &duckdbdriver.Error{
		Msg: "Connection Error: Can't open a connection to same database file with a different configuration than existing connections",
	})
}
