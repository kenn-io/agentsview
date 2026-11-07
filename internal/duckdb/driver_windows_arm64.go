//go:build windows && arm64

package duckdb

import (
	"context"
	"database/sql"
	"errors"
)

// duckdb-go-bindings ships prebuilt static libraries only for
// darwin/{amd64,arm64}, linux/{amd64,arm64}, and windows/amd64. Linking the
// driver on windows/arm64 fails with undefined duckdb_* symbols, so this stub
// keeps the rest of the binary buildable and reports a clear error if the
// DuckDB backend is used.
var errUnsupportedPlatform = errors.New(
	"the DuckDB backend is not supported on windows/arm64: " +
		"duckdb-go-bindings ships no prebuilt DuckDB library for this platform",
)

func openDuckDB(string) (*sql.DB, error) {
	return nil, errUnsupportedPlatform
}

func isMirrorOpenInSameProcessError(error) bool { return false }

type duckAppendBatch struct{}

func newDuckAppendBatch(context.Context, any, map[string][]string) (*duckAppendBatch, error) {
	return nil, errUnsupportedPlatform
}
func (*duckAppendBatch) add(context.Context, string, ...any) error { return errUnsupportedPlatform }
func (*duckAppendBatch) close(context.Context) error               { return errUnsupportedPlatform }
