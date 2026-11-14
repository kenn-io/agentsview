package duckdb

import (
	"os"
	"testing"

	"go.kenn.io/agentsview/internal/testutil"
)

func TestMain(m *testing.M) {
	cleanup := testutil.IsolateCacheDirectory()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
