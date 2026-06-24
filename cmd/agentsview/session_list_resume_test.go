package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// seedActivity seeds a session whose last activity (ended_at) is `ago`
// before now, so the --resume/--active window can be exercised against a
// real clock. user_message_count stays >= 2 to clear the default
// one-shot filter.
func seedActivity(t *testing.T, dataDir, id string, ago time.Duration) {
	t.Helper()
	ts := time.Now().Add(-ago).UTC().Format(time.RFC3339)
	seedSessionWithOpts(t, dataDir, id, "p", func(s *db.Session) {
		s.StartedAt = new(ts)
		s.EndedAt = new(ts)
	})
}

func TestSessionList_ResumeFiltersToActiveWindow(t *testing.T) {
	for _, flag := range []string{"--resume", "--active"} {
		t.Run(flag, func(t *testing.T) {
			dataDir := testDataDir(t)
			seedActivity(t, dataDir, "fresh", 2*time.Minute)
			seedActivity(t, dataDir, "stale", 2*time.Hour)

			out, err := executeCommand(newRootCommand(),
				"session", "list", flag, "--format", "json")
			require.NoError(t, err)

			// Only the session inside the 15m window survives, newest first.
			assert.Equal(t, []string{"fresh"}, sessionListIDs(t, out))
		})
	}
}

func TestSessionList_NoResumeShowsAll(t *testing.T) {
	dataDir := testDataDir(t)
	seedActivity(t, dataDir, "fresh", 2*time.Minute)
	seedActivity(t, dataDir, "stale", 2*time.Hour)

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--format", "json")
	require.NoError(t, err)

	// Without --resume both sessions are listed (recency order).
	assert.Equal(t, []string{"fresh", "stale"}, sessionListIDs(t, out))
}

// TestSessionList_ResumeRespectsExplicitActiveSince guards that an explicit
// --active-since is not clobbered by --resume: the caller can widen the
// window to include older sessions.
func TestSessionList_ResumeRespectsExplicitActiveSince(t *testing.T) {
	dataDir := testDataDir(t)
	seedActivity(t, dataDir, "fresh", 2*time.Minute)
	seedActivity(t, dataDir, "stale", 2*time.Hour)

	wide := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	out, err := executeCommand(newRootCommand(),
		"session", "list", "--resume", "--active-since", wide, "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, []string{"fresh", "stale"}, sessionListIDs(t, out))
}

func TestSessionList_ResumeHumanOutput(t *testing.T) {
	dataDir := testDataDir(t)
	seedActivity(t, dataDir, "fresh", 2*time.Minute)
	seedActivity(t, dataDir, "stale", 2*time.Hour)

	out, err := executeCommand(newRootCommand(), "session", "list", "--resume")
	require.NoError(t, err)
	// Enriched human header is present and the in-flight marker is shown
	// for the recently-active row. The ID column keeps a copyable handle
	// for the surfaced session.
	assert.Contains(t, out, "ID")
	assert.Contains(t, out, "AGE")
	assert.Contains(t, out, "NAME")
	assert.Contains(t, out, "fresh")
	assert.Contains(t, out, activeMarker)
	assert.NotContains(t, out, "stale")
}
