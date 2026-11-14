//go:build chtest

package clickhouse

import (
	"os"
	"testing"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/testutil"
)

func TestMain(m *testing.M) {
	cleanup := testutil.IsolateCacheDirectory()
	code := m.Run()
	chtest.Terminate()
	cleanup()
	os.Exit(code)
}
