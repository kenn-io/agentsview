package main

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestSessionSearchThinkingSource(t *testing.T) {
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")
	dataDir := newAgentDataDir(t)
	archive := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	dbtest.SeedNativeSearch(t, archive)
	stub := stubPGReadStore(t, archive)
	out, err := executeCommand(newRootCommand(), "session", "search", "reasonneedle", "--in", "thinking", "--pg", "--format", "json")
	require.NoError(t, err)
	assert.True(t, stub.Opened)
	assert.True(t, stub.CleanupCalled)
	var result service.ContentSearchResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "thinking", result.Matches[0].Location)
	assert.Equal(t, 1, result.Matches[0].Ordinal)
	assert.NotContains(t, result.Matches[0].Snippet, "AKIA7QHWN2DKR4FYPLJM")
}
