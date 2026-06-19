package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// sessionListIDs decodes `session list --format json` output into ordered IDs.
func sessionListIDs(t *testing.T, out string) []string {
	t.Helper()
	var got struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	ids := make([]string, len(got.Sessions))
	for i, s := range got.Sessions {
		ids[i] = s.ID
	}
	return ids
}

func TestSessionList_SortAndReverse(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSessionWithOpts(t, dataDir, "lo", "p", func(s *db.Session) { s.MessageCount = 2 })
	seedSessionWithOpts(t, dataDir, "mid", "p", func(s *db.Session) { s.MessageCount = 5 })
	seedSessionWithOpts(t, dataDir, "hi", "p", func(s *db.Session) { s.MessageCount = 9 })

	// --sort messages defaults to ascending.
	out, err := executeCommand(newRootCommand(),
		"session", "list", "--sort", "messages", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, []string{"lo", "mid", "hi"}, sessionListIDs(t, out))

	// --reverse flips it to descending.
	out, err = executeCommand(newRootCommand(),
		"session", "list", "--sort", "messages", "--reverse", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, []string{"hi", "mid", "lo"}, sessionListIDs(t, out))

	// -r is the shorthand for --reverse.
	out, err = executeCommand(newRootCommand(),
		"session", "list", "--sort", "messages", "-r", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, []string{"hi", "mid", "lo"}, sessionListIDs(t, out))
}

func TestSessionList_MultiKeySort(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSessionWithOpts(t, dataDir, "a", "p", func(s *db.Session) {
		s.MessageCount = 1
		s.StartedAt = new("2024-03-01T00:00:00Z")
	})
	seedSessionWithOpts(t, dataDir, "b", "p", func(s *db.Session) {
		s.MessageCount = 1
		s.StartedAt = new("2024-01-01T00:00:00Z")
	})
	seedSessionWithOpts(t, dataDir, "c", "p", func(s *db.Session) {
		s.MessageCount = 2
		s.StartedAt = new("2024-02-01T00:00:00Z")
	})

	// Per-key directions: messages asc, then started desc.
	out, err := executeCommand(newRootCommand(),
		"session", "list", "--sort", "messages:asc,started:desc", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, sessionListIDs(t, out))

	// --reverse flips only the unsuffixed key (messages -> desc); the explicit
	// started:asc is left untouched.
	out, err = executeCommand(newRootCommand(),
		"session", "list", "--sort", "messages,started:asc", "-r", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, []string{"c", "b", "a"}, sessionListIDs(t, out))
}

// TestSessionList_EmptySortReverse guards the edge where --sort is explicitly
// cleared: --reverse must still flip the implicit default recent sort (to
// ascending) rather than silently no-opping.
func TestSessionList_EmptySortReverse(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSessionWithOpts(t, dataDir, "old", "p", func(s *db.Session) {
		s.EndedAt = new("2024-01-01T00:00:00Z")
	})
	seedSessionWithOpts(t, dataDir, "new", "p", func(s *db.Session) {
		s.EndedAt = new("2024-03-01T00:00:00Z")
	})

	// Default recent is newest-first.
	out, err := executeCommand(newRootCommand(),
		"session", "list", "--sort", "", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, []string{"new", "old"}, sessionListIDs(t, out))

	// --reverse on the empty (default) sort flips recent to oldest-first.
	out, err = executeCommand(newRootCommand(),
		"session", "list", "--sort", "", "--reverse", "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, []string{"old", "new"}, sessionListIDs(t, out))
}

func TestSessionList_InvalidSort(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-a", "p")

	_, err := executeCommand(newRootCommand(),
		"session", "list", "--sort", "bogus", "--format", "json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid sort")
}
