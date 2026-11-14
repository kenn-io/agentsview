package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestSessionCompactions(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "compacted", "project")
	d, err := db.Open(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{{SessionID: "compacted", Ordinal: 1, Role: "assistant", IsSystem: true, IsCompactBoundary: true, Content: "summary"}}))
	require.NoError(t, d.Close())
	out, err := executeCommand(newRootCommand(), "session", "compactions", "compacted", "--json")
	require.NoError(t, err)
	got := decodeCLIJSON[service.CompactionSummariesResult](t, out)
	require.Len(t, got.Summaries, 1)
	assert.Equal(t, "summary", got.Summaries[0].Content)
	for _, args := range [][]string{{"session", "compactions"}, {"session", "compactions", "compacted", "--active-since", "2024-01-01T00:00:00Z"}, {"session", "compactions", "compacted", "--cursor", "cursor"}} {
		_, err := executeCommand(newRootCommand(), args...)
		require.Error(t, err)
	}
}
