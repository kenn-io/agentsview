package db

import (
	"context"
	"testing"
)

// pinFirstMessage pins the first message of a session and returns
// the message ID. Fails the test if no messages exist.
func pinFirstMessage(t *testing.T, d *DB, sessionID string) int64 {
	t.Helper()
	ctx := context.Background()
	msgs, err := d.GetMessages(ctx, sessionID, 0, 1, true)
	requireNoError(t, err, "GetMessages")
	if len(msgs) == 0 {
		t.Fatalf("no messages in session %s", sessionID)
	}
	id, err := d.PinMessage(sessionID, msgs[0].ID, nil)
	requireNoError(t, err, "PinMessage")
	if id == 0 {
		t.Fatalf("PinMessage returned 0 for session %s msg %d", sessionID, msgs[0].ID)
	}
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
	requireNoError(t, err, "ListPinnedMessages no filter")
	if len(pins) != 2 {
		t.Fatalf("got %d pins, want 2", len(pins))
	}
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
			requireNoError(t, err, "ListPinnedMessages")
			if len(pins) != tc.wantCount {
				t.Errorf("got %d pins, want %d", len(pins), tc.wantCount)
			}
			// Verify project metadata on returned pins matches filter.
			for _, p := range pins {
				if tc.project != "" && p.SessionProject != nil &&
					*p.SessionProject != tc.project {
					t.Errorf("pin session_project = %q, want %q",
						*p.SessionProject, tc.project)
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
	requireNoError(t, err, "soft-delete session")

	pins, err := d.ListPinnedMessages(ctx, "", "alpha")
	requireNoError(t, err, "ListPinnedMessages")
	if len(pins) != 1 {
		t.Fatalf("got %d pins, want 1 (trashed session excluded)", len(pins))
	}
	if pins[0].SessionID != "live" {
		t.Errorf("expected pin from live session, got session_id %q", pins[0].SessionID)
	}
}

func TestListPinnedMessages_SessionFilterIgnoresProject(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "alpha")
	insertMessages(t, d, userMsg("s1", 0, "msg"))
	pinFirstMessage(t, d, "s1")

	// project param is ignored when sessionID is set.
	pins, err := d.ListPinnedMessages(ctx, "s1", "beta")
	requireNoError(t, err, "ListPinnedMessages by session")
	if len(pins) != 1 {
		t.Fatalf("got %d pins, want 1", len(pins))
	}
}
