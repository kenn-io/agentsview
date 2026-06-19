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

func TestSessionList_InvalidSort(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	seedSession(t, dataDir, "s-a", "p")

	_, err := executeCommand(newRootCommand(),
		"session", "list", "--sort", "bogus", "--format", "json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid sort")
}
