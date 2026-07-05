// ABOUTME: `session list --since` relative time filter tests -- flag
// ABOUTME: validation, actual filtering behavior, and --resume interaction.
package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionListSinceMutuallyExclusiveWithActiveSince verifies the error is
// returned before any service/DB access is attempted (no data dir is set up
// here): --since resolution runs ahead of resolveService in the command.
func TestSessionListSinceMutuallyExclusiveWithActiveSince(t *testing.T) {
	_, err := executeCommand(newRootCommand(),
		"session", "list", "--since", "14d",
		"--active-since", "2024-01-01T00:00:00Z")
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"--since and --active-since are mutually exclusive")
}

func TestSessionListSinceRejectsInvalidFormat(t *testing.T) {
	_, err := executeCommand(newRootCommand(),
		"session", "list", "--since", "3x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --since")
}

// TestSessionListSinceFiltersByActivity is the end-to-end regression test:
// --since must narrow results the same way --active-since already does,
// by resolving to an RFC3339 boundary and threading it through unchanged.
func TestSessionListSinceFiltersByActivity(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionsWithOpts(t, dataDir,
		activitySeed("fresh", 2*time.Hour),
		activitySeed("stale", 20*24*time.Hour),
	)

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--since", "7d", "--format", "json")
	require.NoError(t, err)

	assert.Equal(t, []string{"fresh"}, sessionListIDs(t, out))
}

// TestSessionListSinceAcceptsAbsoluteDate covers the YYYY-MM-DD form of
// --since alongside the relative-duration form already covered above.
func TestSessionListSinceAcceptsAbsoluteDate(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionsWithOpts(t, dataDir,
		activitySeed("fresh", 2*time.Hour),
		activitySeed("stale", 20*24*time.Hour),
	)

	yesterday := time.Now().Add(-24 * time.Hour).Format("2006-01-02")
	out, err := executeCommand(newRootCommand(),
		"session", "list", "--since", yesterday, "--format", "json")
	require.NoError(t, err)

	assert.Equal(t, []string{"fresh"}, sessionListIDs(t, out))
}

// TestSessionListResumeRespectsExplicitSince is the CRITICAL interaction
// regression test: --resume/--active push a default 15-minute active_since
// window unless an explicit --active-since was given. That guard must also
// recognize an explicit --since, or a session outside the 15-minute default
// but inside the requested --since window would be silently dropped.
func TestSessionListResumeRespectsExplicitSince(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionsWithOpts(t, dataDir,
		activitySeed("within-since", 2*time.Hour),
		activitySeed("outside-since", 20*24*time.Hour),
	)

	// 2 hours ago is well outside --resume's default 15-minute window, so
	// this would incorrectly return no sessions if --since were overridden.
	out, err := executeCommand(newRootCommand(),
		"session", "list", "--resume", "--since", "1d", "--format", "json")
	require.NoError(t, err)

	assert.Equal(t, []string{"within-since"}, sessionListIDs(t, out))
}

// TestResolveSinceFlag_ResolvesRelativeWindow is a fast unit test of the
// shared flag-resolution helper: since ParseSince resolves against
// time.Now() with no clock-injection seam in this command, the returned
// RFC3339 boundary is asserted to fall within a tolerance window around
// now minus the requested duration rather than an exact instant.
func TestResolveSinceFlag_ResolvesRelativeWindow(t *testing.T) {
	before := time.Now().Add(-14 * 24 * time.Hour)
	got, err := resolveSinceFlag("14d", "")
	after := time.Now().Add(-14 * 24 * time.Hour)
	require.NoError(t, err)

	parsed, err := time.Parse(time.RFC3339, got)
	require.NoError(t, err)
	assert.False(t, parsed.Before(before.Add(-time.Second)),
		"resolved active_since %v earlier than expected window start %v",
		parsed, before)
	assert.False(t, parsed.After(after.Add(time.Second)),
		"resolved active_since %v later than expected window end %v",
		parsed, after)
}

func TestResolveSinceFlag_PassesThroughActiveSinceWhenSinceUnset(t *testing.T) {
	got, err := resolveSinceFlag("", "2024-01-01T00:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, "2024-01-01T00:00:00Z", got)
}

func TestResolveSinceFlag_RejectsBothSet(t *testing.T) {
	_, err := resolveSinceFlag("14d", "2024-01-01T00:00:00Z")
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"--since and --active-since are mutually exclusive")
}
