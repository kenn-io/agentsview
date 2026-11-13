//go:build !duckdb || (windows && arm64)

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
)

func TestDuckDBCommandsFailBeforeWorkWithoutDriver(t *testing.T) {
	for _, args := range [][]string{
		{"replica", "duckdb", "push"},
		{"replica", "duckdb", "status"},
		{"replica", "duckdb", "serve"},
		{"replica", "duckdb", "quack", "serve"},
		{"duckdb", "push"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dataDir := testDataDir(t)
			var stdout, stderr bytes.Buffer
			err := executeCLIWithLegacyFlagCompat(args, &stdout, &stderr)
			require.ErrorIs(t, err, duckdbsync.ErrUnavailable)
			assert.Equal(t, cliUnavailableExitCode, reportCLIError(err, &stderr))
			// Config settings cannot add a driver the build leaves out.
			assert.NotContains(t, stderr.String(), "AGENTSVIEW_DUCKDB_PATH")
			// No daemon, archive, or mirror work file was created.
			entries, readErr := os.ReadDir(dataDir)
			require.NoError(t, readErr)
			assert.Empty(t, entries)
		})
	}
}

func TestDuckDBGroupPrintsHelpWithoutDriver(t *testing.T) {
	testDataDir(t)
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"replica", "duckdb"})
	require.NoError(t, root.ExecuteContext(t.Context()))
	assert.Contains(t, out.String(), "quack")
}
