//go:build !(windows && arm64)

package duckdb

import (
	"database/sql"

	// Registers the "duckdb" database/sql driver, which statically links
	// the prebuilt DuckDB library from duckdb-go-bindings.
	_ "github.com/duckdb/duckdb-go/v2"
)

func openDuckDB(dsn string) (*sql.DB, error) {
	return sql.Open("duckdb", dsn)
}
