package duckdb

import "errors"

// ErrUnavailable reports that this agentsview build cannot open DuckDB: the
// build leaves out the DuckDB driver, or the platform has no DuckDB library.
var ErrUnavailable = errors.New("DuckDB is not available in this agentsview build")
