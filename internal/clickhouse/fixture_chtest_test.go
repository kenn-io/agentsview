//go:build chtest

package clickhouse

import (
	"os"
	"testing"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	chtest.Terminate()
	os.Exit(code)
}
