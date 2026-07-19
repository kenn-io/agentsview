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

func TestListPinCurationForScope(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "alpha")
	insertSession(t, d, "s2", "beta")
	insertMessages(t, d, userMsg("s1", 0, "hello from alpha"))
	insertMessages(t, d, userMsg("s2", 0, "hello from beta"))
	alphaMsgID := pinFirstMessage(t, d, "s1")
	betaMsgID := pinFirstMessage(t, d, "s2")
	note := "beta note"
	_, err := d.PinMessage("s2", betaMsgID, &note)
	require.NoError(t, err, "PinMessage note update")

	all, err := d.ListPinCurationForScope(ctx, nil, nil)
	require.NoError(t, err, "unfiltered scope")
	require.Len(t, all, 2)

	alphaOnly, err := d.ListPinCurationForScope(ctx, []string{"alpha"}, nil)
	require.NoError(t, err, "include alpha")
	require.Len(t, alphaOnly, 1)
	assert.Equal(t, alphaMsgID, alphaOnly[0].MessageID)
	assert.Equal(t, "", alphaOnly[0].Note)

	excludeAlpha, err := d.ListPinCurationForScope(ctx, nil, []string{"alpha"})
	require.NoError(t, err, "exclude alpha")
	require.Len(t, excludeAlpha, 1)
	assert.Equal(t, betaMsgID, excludeAlpha[0].MessageID)
	assert.Equal(t, note, excludeAlpha[0].Note)
}

func TestListPinnedSessionIDsForScope(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "alpha")
	insertSession(t, d, "s2", "beta")
	insertSession(t, d, "s3", "alpha")
	insertMessages(t, d,
		userMsg("s1", 0, "hello from alpha"),
		userMsg("s1", 1, "second alpha message"))
	insertMessages(t, d, userMsg("s2", 0, "hello from beta"))
	insertMessages(t, d, userMsg("s3", 0, "unpinned alpha"))
	// Two pins in s1 must still yield one distinct session id.
	pinFirstMessage(t, d, "s1")
	msgs, err := d.GetMessages(ctx, "s1", 0, 2, true)
	require.NoError(t, err, "GetMessages")
	require.Len(t, msgs, 2)
	_, err = d.PinMessage("s1", msgs[1].ID, nil)
	require.NoError(t, err, "PinMessage second pin")
	pinFirstMessage(t, d, "s2")

	all, err := d.ListPinnedSessionIDsForScope(ctx, nil, nil)
	require.NoError(t, err, "unfiltered scope")
	assert.Equal(t, []string{"s1", "s2"}, all)

	alphaOnly, err := d.ListPinnedSessionIDsForScope(ctx, []string{"alpha"}, nil)
	require.NoError(t, err, "include alpha")
	assert.Equal(t, []string{"s1"}, alphaOnly)

	excludeAlpha, err := d.ListPinnedSessionIDsForScope(ctx, nil, []string{"alpha"})
	require.NoError(t, err, "exclude alpha")
	assert.Equal(t, []string{"s2"}, excludeAlpha)
}

// TestListPinCurationForScopeCapturesIdentityAndNoteNullability is the
// FIX6 regression: a curation fingerprint entry built only from
// (message_id, note-COALESCE'd-to-empty-string) is unchanged by an unpin followed by a
// repin of the same message with the same note, and conflates an explicit
// empty-string note with no note at all. PinCurationEntry must carry the
// pin's own row id, created_at, and note-presence separately from note
// content so both cases are distinguishable.
//
// A second, untouched pin (s2) is kept present throughout so
// pinned_messages is never fully empty: SQLite recycles the highest unused
// rowid for a plain INTEGER PRIMARY KEY once a table has no rows at all, so
// deleting the sole row and reinserting one could otherwise reuse the same
// id purely as an artifact of the table being momentarily empty, rather
// than demonstrating anything about the fix.
func TestListPinCurationForScopeCapturesIdentityAndNoteNullability(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "alpha")
	insertSession(t, d, "s2", "alpha")
	insertMessages(t, d, userMsg("s1", 0, "hello"))
	insertMessages(t, d, userMsg("s2", 0, "world"))
	msgID := pinFirstMessage(t, d, "s1") // pinned with a nil note
	pinFirstMessage(t, d, "s2")

	entries, err := d.ListPinCurationForScope(ctx, nil, nil)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	before := findPinCurationEntry(t, entries, msgID)
	require.NotZero(t, before.ID)
	assert.False(t, before.HasNote,
		"a pin created without a note must not read back as an empty-string note")
	assert.Equal(t, "", before.Note)
	assert.NotEmpty(t, before.CreatedAt)

	require.NoError(t, d.UnpinMessage("s1", msgID))
	emptyNote := ""
	_, err = d.PinMessage("s1", msgID, &emptyNote)
	require.NoError(t, err)

	entries, err = d.ListPinCurationForScope(ctx, nil, nil)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	after := findPinCurationEntry(t, entries, msgID)
	assert.NotEqual(t, before.ID, after.ID,
		"unpin+repin must produce a new pin row id even though message_id is unchanged")
	assert.True(t, after.HasNote,
		"an explicit empty-string note is a different state than no note at all")
	assert.Equal(t, "", after.Note)
}

func findPinCurationEntry(
	t *testing.T, entries []PinCurationEntry, messageID int64,
) PinCurationEntry {
	t.Helper()
	for _, e := range entries {
		if e.MessageID == messageID {
			return e
		}
	}
	t.Fatalf("no pin curation entry for message %d", messageID)
	return PinCurationEntry{}
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
