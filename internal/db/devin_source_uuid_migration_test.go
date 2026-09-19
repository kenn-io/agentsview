package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenScopesLegacyDevinSourceUUIDs covers an archive written before
// data version 111: Devin rows store bare node/step ids, which the
// migration rewrites in place to the session-scoped form the parser now
// emits. Local and remote Devin sessions are rewritten, already-scoped
// values and non-Devin sessions are untouched, rewritten sessions get a
// transcript revision bump, and a repeated stale open is a no-op.
func TestOpenScopesLegacyDevinSourceUUIDs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	d, err := Open(path)
	require.NoError(t, err, "initial open")
	for _, id := range []string{
		"devin:sess-a", "host~devin:sess-b", "host~other:sess-c",
	} {
		insertSession(t, d, id, "proj", func(s *Session) {
			s.Agent = "devin"
		})
		insertMessages(t, d,
			Message{
				SessionID: id, Ordinal: 0, Role: "user",
				Content: "task", Timestamp: tsZero,
				SourceUUID: "2",
			},
			Message{
				SessionID: id, Ordinal: 1, Role: "assistant",
				Content: "working", Timestamp: tsZero,
				SourceUUID: "3", SourceParentUUID: "2",
			},
			Message{
				SessionID: id, Ordinal: 2, Role: "assistant",
				Content: "done", Timestamp: tsZero,
				SourceUUID: "sess-x:4",
			},
		)
	}
	for _, id := range []string{
		"devin:sess-a", "host~devin:sess-b", "host~other:sess-c",
	} {
		_, err = d.getWriter().Exec(`
			INSERT INTO recall_entries (
				id, type, scope, title, body, source_session_id
			) VALUES (?, 'fact', 'project', 't', 'b', ?)`,
			"entry-"+id, id,
		)
		require.NoError(t, err, "insert recall entry")
		_, err = d.getWriter().Exec(`
			INSERT INTO recall_evidence (
				entry_id, session_id, message_start_ordinal,
				message_end_ordinal, message_start_source_uuid,
				message_end_source_uuid
			) VALUES (?, ?, 0, 1, '2', '3')`,
			"entry-"+id, id,
		)
		require.NoError(t, err, "insert recall evidence")
	}
	msgs, err := d.GetAllMessages(ctx, "devin:sess-a")
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	_, err = d.PinMessage("devin:sess-a", msgs[1].ID, nil)
	require.NoError(t, err, "PinMessage")
	revisionBefore := map[string]string{}
	for _, id := range []string{
		"devin:sess-a", "host~devin:sess-b", "host~other:sess-c",
	} {
		s, err := d.GetSession(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, s.TranscriptRevision)
		revisionBefore[id] = *s.TranscriptRevision
	}
	d.Close()

	setUserVersion := func(v int) {
		conn, err := sql.Open("sqlite3", path)
		require.NoError(t, err, "raw open")
		_, err = conn.Exec("PRAGMA user_version = " + itoa(v))
		require.NoError(t, err, "set version")
		conn.Close()
	}
	setUserVersion(devinSourceUUIDScopeVersion - 1)

	d, err = Open(path)
	require.NoError(t, err, "stale reopen")
	defer d.Close()

	tests := []struct {
		sessionID  string
		wantUUIDs  []string
		wantParent string
		wantStart  string
		wantEnd    string
		rewritten  bool
	}{
		{"devin:sess-a",
			[]string{"sess-a:2", "sess-a:3", "sess-x:4"},
			"sess-a:2", "sess-a:2", "sess-a:3", true},
		{"host~devin:sess-b",
			[]string{"sess-b:2", "sess-b:3", "sess-x:4"},
			"sess-b:2", "sess-b:2", "sess-b:3", true},
		{"host~other:sess-c",
			[]string{"2", "3", "sess-x:4"}, "2", "2", "3", false},
	}
	for _, tt := range tests {
		t.Run(tt.sessionID, func(t *testing.T) {
			msgs, err := d.GetAllMessages(ctx, tt.sessionID)
			require.NoError(t, err)
			require.Len(t, msgs, 3)
			got := make([]string, len(msgs))
			for i, m := range msgs {
				got[i] = m.SourceUUID
			}
			assert.Equal(t, tt.wantUUIDs, got)
			assert.Equal(t, tt.wantParent, msgs[1].SourceParentUUID)
			var startUUID, endUUID string
			require.NoError(t, d.getReader().QueryRow(
				`SELECT message_start_source_uuid,
					message_end_source_uuid
				 FROM recall_evidence WHERE entry_id = ?`,
				"entry-"+tt.sessionID,
			).Scan(&startUUID, &endUUID), "read recall endpoints")
			assert.Equal(t, tt.wantStart, startUUID,
				"recall start endpoint must follow the rewrite")
			assert.Equal(t, tt.wantEnd, endUUID,
				"recall end endpoint must follow the rewrite")
			s, err := d.GetSession(ctx, tt.sessionID)
			require.NoError(t, err)
			require.NotNil(t, s.TranscriptRevision)
			if tt.rewritten {
				assert.NotEqual(t, revisionBefore[tt.sessionID],
					*s.TranscriptRevision,
					"rewritten session must advance its revision")
			} else {
				assert.Equal(t, revisionBefore[tt.sessionID],
					*s.TranscriptRevision)
			}
		})
	}

	// The resync re-parse stores the same messages under scoped uuids.
	// Because the archive already holds that form, the replace path keeps
	// the pin attached without any identity translation.
	require.NoError(t, d.ReplaceSessionMessages("devin:sess-a", []Message{
		{SessionID: "devin:sess-a", Ordinal: 0, Role: "user",
			Content: "task", Timestamp: tsZero, SourceUUID: "sess-a:2"},
		{SessionID: "devin:sess-a", Ordinal: 1, Role: "assistant",
			Content: "working", Timestamp: tsZero,
			SourceUUID: "sess-a:3", SourceParentUUID: "sess-a:2"},
		{SessionID: "devin:sess-a", Ordinal: 2, Role: "assistant",
			Content: "done", Timestamp: tsZero, SourceUUID: "sess-x:4"},
	}), "ReplaceSessionMessages")
	pins, err := d.ListPinnedMessages(ctx, "devin:sess-a", "")
	require.NoError(t, err)
	require.Len(t, pins, 1, "pin must survive the scoped re-parse")
	assert.Equal(t, 1, pins[0].Ordinal)

	// A second stale open (the resync has not stamped the version yet)
	// finds nothing bare and leaves revisions alone.
	s, err := d.GetSession(ctx, "host~devin:sess-b")
	require.NoError(t, err)
	revisionAfter := *s.TranscriptRevision
	d.Close()
	d, err = Open(path)
	require.NoError(t, err, "second stale reopen")
	defer d.Close()
	s, err = d.GetSession(ctx, "host~devin:sess-b")
	require.NoError(t, err)
	assert.Equal(t, revisionAfter, *s.TranscriptRevision,
		"repeated migration must be a no-op")
}
