//go:build !duckdb || (windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"errors"
)

// Default builds leave out the DuckDB driver. DuckDB 2.0's prebuilt libraries
// require an x86-64-v3 CPU (AVX2, BMI2, FMA), and the bindings call into them
// from a package init, so linking them crashed every agentsview start on older
// x86-64 CPUs, even for users who never touch the DuckDB mirror. Building with
// -tags duckdb links the driver. duckdb-go-bindings ships no prebuilt library
// for windows/arm64, so that platform always gets this stub.
var errDuckDBUnavailable = errors.New(
	"this agentsview build does not include DuckDB: " +
		"build agentsview from source with `make install DUCKDB=1` (or -tags fts5,duckdb) " +
		"to use the DuckDB mirror; see https://agentsview.io/docs/duckdb/#build-with-duckdb",
)

// RequireDriver reports an error when this build does not link the DuckDB
// driver.
func RequireDriver() error { return errDuckDBUnavailable }

func openDuckDB(string) (*sql.DB, error) {
	return nil, errDuckDBUnavailable
}

func isMirrorOpenInSameProcessError(error) bool { return false }

type duckAppendBatch struct{}

func newDuckAppendBatch(context.Context, any, map[string][]columnSpec) (*duckAppendBatch, error) {
	return nil, errDuckDBUnavailable
}
func (*duckAppendBatch) add(context.Context, string, ...any) error { return errDuckDBUnavailable }
func (*duckAppendBatch) flush(context.Context) error               { return errDuckDBUnavailable }
func (*duckAppendBatch) close(context.Context) error               { return errDuckDBUnavailable }
