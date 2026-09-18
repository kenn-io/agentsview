package db

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func messageCountWrite(id string, count int) SessionBatchWrite {
	messages := make([]Message, count)
	for i := range messages {
		content := fmt.Sprintf("message-%03d", i)
		messages[i] = Message{
			SessionID:     id,
			Ordinal:       i,
			Role:          "user",
			Content:       content,
			ContentLength: len(content),
			Timestamp:     time.Unix(int64(i), 0).UTC().Format(time.RFC3339),
		}
	}
	return SessionBatchWrite{
		Session: Session{
			ID: id, Project: "project", Machine: defaultMachine,
			Agent: "claude", MessageCount: count, UserMessageCount: count,
		},
		Messages:        messages,
		DataVersion:     CurrentDataVersion(),
		ReplaceMessages: true,
	}
}

func requireSessionMessageCount(t *testing.T, d *DB, id string, want int) {
	t.Helper()
	messages, err := d.GetAllMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, messages, want)
}

func TestWriteSessionBatchMessageCountCondition(t *testing.T) {
	tests := []struct {
		name     string
		incoming int
		guard    bool
		wantErr  bool
	}{
		{name: "shorter rejected", incoming: 24, guard: true, wantErr: true},
		{name: "equal allowed", incoming: 96, guard: true},
		{name: "longer allowed", incoming: 120, guard: true},
		{name: "zero value preserves replacement", incoming: 24},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)

			d := testDB(t)
			_, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{
				messageCountWrite("session", 96),
			})
			require.NoError(err)

			write := messageCountWrite("session", tt.incoming)
			write.RejectMessageCountDecrease = tt.guard
			_, err = d.WriteSessionBatchAtomic([]SessionBatchWrite{write})
			if !tt.wantErr {
				require.NoError(err)
				requireSessionMessageCount(t, d, "session", tt.incoming)
				return
			}

			var shorter *SessionWouldShortenError
			require.ErrorAs(err, &shorter)
			require.Equal("session", shorter.SessionID)
			require.Equal(96, shorter.ExistingMessages)
			require.Equal(24, shorter.IncomingMessages)
			requireSessionMessageCount(t, d, "session", 96)
		})
	}
}

func TestWriteSessionBatchAtomicShorterMemberRollsBack(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	root := messageCountWrite("root", 96)
	child := messageCountWrite("child", 30)
	child.Session.ParentSessionID = Ptr("root")
	child.Session.RelationshipType = "subagent"
	_, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{root, child})
	require.NoError(err)

	root = messageCountWrite("root", 120)
	child = messageCountWrite("child", 10)
	root.RejectMessageCountDecrease = true
	child.RejectMessageCountDecrease = true
	callbackCalled := false
	result, err := d.WriteSessionBatchAtomic(
		[]SessionBatchWrite{root, child},
		func() error {
			callbackCalled = true
			return nil
		},
	)
	var shorter *SessionWouldShortenError
	require.ErrorAs(err, &shorter)
	require.Equal("child", shorter.SessionID)
	require.Zero(result.WrittenSessions)
	require.False(callbackCalled)
	requireSessionMessageCount(t, d, "root", 96)
	requireSessionMessageCount(t, d, "child", 30)
}

func TestWriteSessionBatchMessageCountDecisionIsSerialized(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	_, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{
		messageCountWrite("session", 96),
	})
	require.NoError(err)

	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	first := messageCountWrite("session", 120)
	first.RejectMessageCountDecrease = true
	go func() {
		_, err := d.WriteSessionBatchAtomic(
			[]SessionBatchWrite{first},
			func() error {
				close(entered)
				<-release
				return nil
			},
		)
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first writer did not reach beforeCommit")
	}

	secondDone := make(chan error, 1)
	second := messageCountWrite("session", 24)
	second.RejectMessageCountDecrease = true
	go func() {
		_, err := d.WriteSessionBatchAtomic([]SessionBatchWrite{second})
		secondDone <- err
	}()
	close(release)
	require.NoError(<-firstDone)

	var shorter *SessionWouldShortenError
	require.ErrorAs(<-secondDone, &shorter)
	require.Equal(120, shorter.ExistingMessages)
	require.Equal(24, shorter.IncomingMessages)
	requireSessionMessageCount(t, d, "session", 120)
}

func TestWriteSessionBatchInsertSkipsRedundantModifiedTouch(t *testing.T) {
	require := require.New(t)

	d := testDB(t)
	_, err := d.getWriter().Exec(`
		CREATE TABLE modified_touch_log(session_id TEXT);
		CREATE TRIGGER trg_modified_touch_log
		AFTER UPDATE OF local_modified_at ON sessions
		BEGIN
			INSERT INTO modified_touch_log VALUES (NEW.id);
		END`)
	require.NoError(err, "install touch-counting trigger")
	touches := func() int {
		var count int
		require.NoError(d.getReader().QueryRow(
			`SELECT count(*) FROM modified_touch_log`,
		).Scan(&count), "count local_modified_at touches")
		return count
	}
	resetTouches := func() {
		_, err := d.getWriter().Exec(`DELETE FROM modified_touch_log`)
		require.NoError(err, "reset touch log")
	}

	// A newly inserted session already fires the sync_marker INSERT
	// trigger, so its revision bump must not touch local_modified_at.
	// Replacing an existing transcript must add exactly that one touch
	// so push targets re-select the session.
	_, err = d.WriteSessionBatchAtomic([]SessionBatchWrite{
		messageCountWrite("session", 4),
	})
	require.NoError(err)
	insertTouches := touches()

	resetTouches()
	_, err = d.WriteSessionBatchAtomic([]SessionBatchWrite{
		messageCountWrite("session", 6),
	})
	require.NoError(err)
	require.Equal(insertTouches+1, touches(),
		"revision bump must touch local_modified_at only for replacements")

	var modifiedAt sql.NullString
	require.NoError(d.getReader().QueryRow(
		`SELECT local_modified_at FROM sessions WHERE id = 'session'`,
	).Scan(&modifiedAt), "read local_modified_at")
	require.True(modifiedAt.Valid && modifiedAt.String != "",
		"batch-written session must carry local_modified_at")
}
