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
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"session", "compactions", "compacted", "--limit", "-1"}, "--limit must be 0 or more"},
		{[]string{"session", "compactions"}, "provide exactly one session ID or --active-since"},
		{[]string{"session", "compactions", "compacted", "--active-since", "2024-01-01T00:00:00Z"}, "provide exactly one session ID or --active-since"},
	} {
		var stdout, stderr bytes.Buffer
		err := executeCLIWithLegacyFlagCompat(tc.args, &stdout, &stderr)
		require.Error(t, err)
		assert.Equal(t, 64, reportCLIError(err, &stderr))
		assert.Contains(t, stderr.String(), "fatal: "+tc.message+"\n")
	}
}

func TestSessionCompactionsCursorWithID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := executeCLIWithLegacyFlagCompat([]string{"session", "compactions", "compacted", "--cursor", "cursor"}, &stdout, &stderr)
	require.Error(t, err)
	assert.Equal(t, 64, reportCLIError(err, &stderr))
	assert.Equal(t, "fatal: --cursor requires --active-since\nhint: run `agentsview session compactions --help` for flags and examples\n", stderr.String())
}

func TestSessionCompactionsOutput(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionsWithOpts(t, dataDir,
		sessionSeed{id: "first", project: "project", mut: func(s *db.Session) { s.EndedAt = new("2024-06-16T00:00:00Z") }},
		sessionSeed{id: "second", project: "project", mut: func(s *db.Session) { s.EndedAt = new("2024-06-15T00:00:00Z") }},
		sessionSeed{id: "evener", project: "project", mut: func(s *db.Session) { s.Agent = "evener"; s.EndedAt = new("2024-06-14T00:00:00Z") }},
	)
	d, err := db.Open(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "first", Role: "assistant", IsSystem: true, IsCompactBoundary: true, Content: "first summary"},
		{SessionID: "second", Role: "assistant", IsSystem: true, IsCompactBoundary: true, Content: "second summary"},
		{SessionID: "evener", Role: "system", IsSystem: true, IsCompactBoundary: true, SourceType: "entry", SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
	}))
	require.NoError(t, d.Close())
	out, err := executeCommand(newRootCommand(), "session", "compactions", "--active-since", "2024-06-01T00:00:00Z", "--limit", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "first summary")
	_, cursor, found := strings.Cut(out, "More results: --cursor ")
	require.True(t, found)
	cursor = strings.TrimSpace(cursor)
	require.NotEmpty(t, cursor)
	out, err = executeCommand(newRootCommand(), "session", "compactions", "--active-since", "2024-06-01T00:00:00Z", "--limit", "1", "--cursor", cursor)
	require.NoError(t, err)
	assert.Contains(t, out, "second summary")
	out, err = executeCommand(newRootCommand(), "session", "compactions", "evener")
	require.NoError(t, err)
	assert.Contains(t, out, "Ambiguous compaction label: evener #0")
}
