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
	unavailable := duckdbsync.RequireDriver()
	require.Error(t, unavailable)
	for _, args := range [][]string{
		{"replica", "duckdb", "push"},
		{"replica", "duckdb", "status"},
		{"replica", "duckdb", "serve"},
		{"replica", "duckdb", "quack", "serve"},
		{"duckdb", "push"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dataDir := testDataDir(t)
			root := newRootCommand()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(args)
			err := root.ExecuteContext(t.Context())
			require.ErrorIs(t, err, unavailable)
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
