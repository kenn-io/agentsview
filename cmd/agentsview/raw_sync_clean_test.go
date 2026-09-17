package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawSyncRegistersCleanUploads(t *testing.T) {
	help, err := executeCommand(newRootCommand(), "raw-sync", "--help")

	require.NoError(t, err)
	assert.Contains(t, help, "clean-uploads")
}

func TestRawSyncCleanUploadsHelp(t *testing.T) {
	help, err := executeCommand(
		newRootCommand(), "raw-sync", "clean-uploads", "--help",
	)

	require.NoError(t, err)
	assert.Contains(t, help, "Run one bounded server upload cleanup pass")
	assert.Contains(t, help, "configured PostgreSQL target and AgentsView data directory")
}

func TestRawSyncCleanUploadsRejectsArguments(t *testing.T) {
	testDataDir(t)

	output, err := executeCommand(
		newRootCommand(), "raw-sync", "clean-uploads", "unexpected",
	)

	require.Error(t, err)
	assert.Empty(t, output)
	assert.Contains(t, err.Error(), "unknown command \"unexpected\"")
}

func TestRawSyncCleanUploadsRequiresPostgreSQL(t *testing.T) {
	testDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "")

	output, err := executeCommand(newRootCommand(), "raw-sync", "clean-uploads")

	require.Error(t, err)
	assert.Empty(t, output)
	assert.Contains(t, err.Error(), "postgres URL is required")
	assert.NotContains(t, err.Error(), rawSyncCleanUploadsCompletion)
}
