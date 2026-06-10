package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinFirstMessage pins the first message of a session and returns
// the message ID. Fails the test if no messages exist.
func pinFirstMessage(t *testing.T, d *DB, sessionID string) int64 {
	t.Helper()
	ctx := context.Background()
	msgs, err := d.GetMessages(ctx, sessionID, 0, 1, true)
	require.NoError(t, err, "GetMessages")
	require.NotEmpty(t, msgs, "no messages in session %s", sessionID)
	id, err := d.PinMessage(sessionID, msgs[0].ID, nil)
	require.NoError(t, err, "PinMessage")
	require.NotZero(t, id, "PinMessage returned 0 for session %s msg %d", sessionID, msgs[0].ID)
	return msgs[0].ID
}

func TestListPinnedMessages_NoFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "alpha")
	insertSession(t, d, "s2", "beta")
	insertMessages(t, d, userMsg("s1", 0, "hello from alpha"))
	insertMessages(t, d, userMsg("s2", 0, "hello from beta"))
	pinFirstMessage(t, d, "s1")
	pinFirstMessage(t, d, "s2")

	pins, err := d.ListPinnedMessages(ctx, "", "")
	require.NoError(t, err, "ListPinnedMessages no filter")
	require.Len(t, pins, 2)
}

func TestListPinnedMessages_ProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "alpha")
	insertSession(t, d, "s2", "alpha")
	insertSession(t, d, "s3", "beta")
	insertMessages(t, d, userMsg("s1", 0, "alpha msg 1"))
	insertMessages(t, d, userMsg("s2", 0, "alpha msg 2"))
	insertMessages(t, d, userMsg("s3", 0, "beta msg"))
	pinFirstMessage(t, d, "s1")
	pinFirstMessage(t, d, "s2")
	pinFirstMessage(t, d, "s3")

	tests := []struct {
		project   string
		wantCount int
	}{
		{"alpha", 2},
		{"beta", 1},
		{"unknown", 0},
		{"", 3},
	}
	for _, tc := range tests {
		t.Run("project="+tc.project, func(t *testing.T) {
			pins, err := d.ListPinnedMessages(ctx, "", tc.project)
			require.NoError(t, err, "ListPinnedMessages")
			assert.Len(t, pins, tc.wantCount)
			// Verify project metadata on returned pins matches filter.
			for _, p := range pins {
				if tc.project != "" && p.SessionProject != nil {
					assert.Equal(t, tc.project, *p.SessionProject,
						"pin session_project")
				}
			}
		})
	}
}

func TestListPinnedMessages_ProjectFilterExcludesTrashed(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "live", "alpha")
	insertSession(t, d, "trashed", "alpha")
	insertMessages(t, d, userMsg("live", 0, "live msg"))
	insertMessages(t, d, userMsg("trashed", 0, "trashed msg"))
	pinFirstMessage(t, d, "live")
	pinFirstMessage(t, d, "trashed")

	// Soft-delete the trashed session.
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET deleted_at = ? WHERE id = ?",
		tsZeroS1, "trashed",
	)
	require.NoError(t, err, "soft-delete session")

	pins, err := d.ListPinnedMessages(ctx, "", "alpha")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 1, "trashed session excluded")
	assert.Equal(t, "live", pins[0].SessionID,
		"expected pin from live session")
}

func TestListPinnedMessages_SessionFilterIgnoresProject(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "alpha")
	insertMessages(t, d, userMsg("s1", 0, "msg"))
	pinFirstMessage(t, d, "s1")

	// project param is ignored when sessionID is set.
	pins, err := d.ListPinnedMessages(ctx, "s1", "beta")
	require.NoError(t, err, "ListPinnedMessages by session")
	require.Len(t, pins, 1)
}

// TestListPinnedMessages_SessionNameFallback verifies that a session
// with only session_name set (no user rename / display_name) returns
// the session_name value in SessionDisplayName rather than NULL.
func TestListPinnedMessages_SessionNameFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	agentName := "My Agent Session"
	insertSession(t, d, "s1", "alpha", func(s *Session) {
		s.SessionName = &agentName
		// DisplayName intentionally left nil (no user rename).
	})
	insertMessages(t, d, userMsg("s1", 0, "hello from agent"))
	pinFirstMessage(t, d, "s1")

	pins, err := d.ListPinnedMessages(ctx, "", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 1)

	require.NotNil(t, pins[0].SessionDisplayName,
		"SessionDisplayName should fall back to session_name, got nil")
	assert.Equal(t, agentName, *pins[0].SessionDisplayName,
		"SessionDisplayName should equal session_name when display_name is NULL")
}
