package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestSessionCompactions(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionsWithOpts(t, dataDir,
		sessionSeed{id: "compacted", project: "project", mut: func(s *db.Session) { s.EndedAt = new("2024-06-16T00:00:00Z") }},
		sessionSeed{id: "second", project: "project", mut: func(s *db.Session) { s.EndedAt = new("2024-06-15T00:00:00Z") }},
		sessionSeed{id: "evener:session", project: "project", mut: func(s *db.Session) { s.Agent = "evener"; s.EndedAt = new("2024-06-14T00:00:00Z") }},
	)
	d, err := db.Open(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "compacted", Ordinal: 1, Role: "assistant", IsSystem: true, IsCompactBoundary: true, Content: "summary"},
		{SessionID: "second", Role: "assistant", IsSystem: true, IsCompactBoundary: true, Content: "second summary"},
		{SessionID: "evener:session", Role: "system", IsSystem: true, IsCompactBoundary: true, SourceType: "entry", SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
	}))
	require.NoError(t, d.Close())
	t.Run("session JSON", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "session", "compactions", "compacted", "--json")
		require.NoError(t, err)
		got := decodeCLIJSON[service.CompactionSummariesResult](t, out)
		require.Len(t, got.Summaries, 1)
		assert.Equal(t, "summary", got.Summaries[0].Content)
	})
	t.Run("cursor", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "session", "compactions", "--active-since", "2024-06-01T00:00:00Z", "--limit", "1")
		require.NoError(t, err)
		_, cursor, found := strings.Cut(out, "More results: --cursor ")
		require.True(t, found)
		out, err = executeCommand(newRootCommand(), "session", "compactions", "--active-since", "2024-06-01T00:00:00Z", "--limit", "1", "--cursor", strings.TrimSpace(cursor))
		require.NoError(t, err)
		assert.Contains(t, out, "second summary")
	})
	t.Run("ambiguous label", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "session", "compactions", "evener:session")
		require.NoError(t, err)
		assert.Contains(t, out, "Ambiguous compaction label: evener:session #0")
	})
	t.Run("usage error", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		err := executeCLIWithLegacyFlagCompat([]string{"session", "compactions", "compacted", "--limit", "-1"}, &stdout, &stderr)
		require.Error(t, err)
		assert.Equal(t, 64, reportCLIError(err, &stderr))
		assert.Contains(t, stderr.String(), "fatal: --limit must be 0 or more\n")
	})
}
