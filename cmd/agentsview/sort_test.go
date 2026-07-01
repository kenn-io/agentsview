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

func TestSessionList_SortFixture(t *testing.T) {
	dataDir := testDataDir(t)
	seedSessionsWithOpts(t, dataDir,
		sessionSeed{id: "lo", project: "sort-count", mut: func(s *db.Session) {
			s.MessageCount = 2
		}},
		sessionSeed{id: "mid", project: "sort-count", mut: func(s *db.Session) {
			s.MessageCount = 5
		}},
		sessionSeed{id: "hi", project: "sort-count", mut: func(s *db.Session) {
			s.MessageCount = 9
		}},
		sessionSeed{id: "a", project: "sort-multi", mut: func(s *db.Session) {
			s.MessageCount = 1
			s.StartedAt = new("2024-03-01T00:00:00Z")
		}},
		sessionSeed{id: "b", project: "sort-multi", mut: func(s *db.Session) {
			s.MessageCount = 1
			s.StartedAt = new("2024-01-01T00:00:00Z")
		}},
		sessionSeed{id: "c", project: "sort-multi", mut: func(s *db.Session) {
			s.MessageCount = 2
			s.StartedAt = new("2024-02-01T00:00:00Z")
		}},
		sessionSeed{id: "old", project: "sort-empty", mut: func(s *db.Session) {
			s.EndedAt = new("2024-01-01T00:00:00Z")
		}},
		sessionSeed{id: "new", project: "sort-empty", mut: func(s *db.Session) {
			s.EndedAt = new("2024-03-01T00:00:00Z")
		}},
	)

	t.Run("sort and reverse", func(t *testing.T) {
		// --sort messages defaults to ascending.
		out, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-count",
			"--sort", "messages", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"lo", "mid", "hi"},
			sessionListIDs(t, out))

		// --reverse flips it to descending.
		out, err = executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-count",
			"--sort", "messages", "--reverse", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"hi", "mid", "lo"},
			sessionListIDs(t, out))

		// -r is the shorthand for --reverse.
		out, err = executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-count",
			"--sort", "messages", "-r", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"hi", "mid", "lo"},
			sessionListIDs(t, out))
	})

	t.Run("multi-key sort", func(t *testing.T) {
		// Per-key directions: messages asc, then started desc.
		out, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-multi",
			"--sort", "messages:asc,started:desc", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b", "c"},
			sessionListIDs(t, out))

		// --reverse flips only the unsuffixed key (messages -> desc); the
		// explicit started:asc is left untouched.
		out, err = executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-multi",
			"--sort", "messages,started:asc", "-r", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"c", "b", "a"},
			sessionListIDs(t, out))
	})

	t.Run("empty sort reverse", func(t *testing.T) {
		// Default recent is newest-first.
		out, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-empty",
			"--sort", "", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"new", "old"}, sessionListIDs(t, out))

		// --reverse on the empty (default) sort flips recent to
		// oldest-first.
		out, err = executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-empty",
			"--sort", "", "--reverse", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"old", "new"}, sessionListIDs(t, out))
	})

	t.Run("invalid sort", func(t *testing.T) {
		_, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-count",
			"--sort", "bogus", "--format", "json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid sort")
	})
}
