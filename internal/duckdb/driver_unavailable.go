//go:build !duckdb || (windows && arm64)

package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
)

// Default builds leave out the DuckDB driver. DuckDB 2.0's prebuilt libraries
// require an x86-64-v3 CPU (AVX2, BMI2, FMA), and the bindings call into them
// from a package init, so linking them crashed every agentsview start on older
// x86-64 CPUs, even for users who never touch the DuckDB mirror. Building with
// -tags duckdb links the driver. duckdb-go-bindings ships no prebuilt library
// for windows/arm64, so that platform always gets this stub.
var errDuckDBUnavailable = unavailableError()

func unavailableError() error {
	if runtime.GOOS == "windows" && runtime.GOARCH == "arm64" {
		return fmt.Errorf("%w: duckdb-go-bindings ships no DuckDB library for windows/arm64", ErrUnavailable)
	}
	return fmt.Errorf("%w: build agentsview from source with `make install DUCKDB=1` "+
		"(or -tags fts5,duckdb); see https://agentsview.io/docs/duckdb/#build-with-duckdb", ErrUnavailable)
}

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
