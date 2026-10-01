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
			d := testDB(t)
			_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{
				messageCountWrite("session", 96),
			})
			require.NoError(t, err)

			write := messageCountWrite("session", tt.incoming)
			write.RejectMessageCountDecrease = tt.guard
			_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{write})
			if !tt.wantErr {
				require.NoError(t, err)
				requireSessionMessageCount(t, d, "session", tt.incoming)
				return
			}

			var shorter *SessionWouldShortenError
			require.ErrorAs(t, err, &shorter)
			require.Equal(t, "session", shorter.SessionID)
			require.Equal(t, 96, shorter.ExistingMessages)
			require.Equal(t, 24, shorter.IncomingMessages)
			requireSessionMessageCount(t, d, "session", 96)
		})
	}
}

func TestWriteSessionBatchAtomicShorterMemberRollsBack(t *testing.T) {
	d := testDB(t)
	root := messageCountWrite("root", 96)
	child := messageCountWrite("child", 30)
	child.Session.ParentSessionID = Ptr("root")
	child.Session.RelationshipType = "subagent"
	_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{root, child})
	require.NoError(t, err)

	root = messageCountWrite("root", 120)
	child = messageCountWrite("child", 10)
	root.RejectMessageCountDecrease = true
	child.RejectMessageCountDecrease = true
	callbackCalled := false
	result, err := d.WriteSessionBatchAtomic(t.Context(),
		[]SessionBatchWrite{root, child},
		func() error {
			callbackCalled = true
			return nil
		},
	)
	var shorter *SessionWouldShortenError
	require.ErrorAs(t, err, &shorter)
	require.Equal(t, "child", shorter.SessionID)
	require.Zero(t, result.WrittenSessions)
	require.False(t, callbackCalled)
	requireSessionMessageCount(t, d, "root", 96)
	requireSessionMessageCount(t, d, "child", 30)
}

func TestWriteSessionBatchMessageCountDecisionIsSerialized(t *testing.T) {
	d := testDB(t)
	_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{
		messageCountWrite("session", 96),
	})
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	first := messageCountWrite("session", 120)
	first.RejectMessageCountDecrease = true
	go func() {
		_, err := d.WriteSessionBatchAtomic(t.Context(),
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
		require.Fail(t, "first writer did not reach beforeCommit")
	}

	secondDone := make(chan error, 1)
	second := messageCountWrite("session", 24)
	second.RejectMessageCountDecrease = true
	go func() {
		_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{second})
		secondDone <- err
	}()
	close(release)
	require.NoError(t, <-firstDone)

	var shorter *SessionWouldShortenError
	require.ErrorAs(t, <-secondDone, &shorter)
	require.Equal(t, 120, shorter.ExistingMessages)
	require.Equal(t, 24, shorter.IncomingMessages)
	requireSessionMessageCount(t, d, "session", 120)
}

func TestWriteSessionBatchInsertSkipsRedundantModifiedTouch(t *testing.T) {
	d := testDB(t)
	_, err := d.getWriter().Exec(t.Context(), `
		CREATE TABLE modified_touch_log(session_id TEXT);
		CREATE TRIGGER trg_modified_touch_log
		AFTER UPDATE OF local_modified_at ON sessions
		BEGIN
			INSERT INTO modified_touch_log VALUES (NEW.id);
		END`)
	require.NoError(t, err, "install touch-counting trigger")
	touches := func() int {
		var count int
		require.NoError(t, d.getReader().QueryRow(t.Context(),
			`SELECT count(*) FROM modified_touch_log`,
		).Scan(&count), "count local_modified_at touches")
		return count
	}
	resetTouches := func() {
		_, err := d.getWriter().Exec(t.Context(), `DELETE FROM modified_touch_log`)
		require.NoError(t, err, "reset touch log")
	}

	// A newly inserted session already fires the sync_marker INSERT
	// trigger, so its revision bump must not touch local_modified_at.
	// Replacing an existing transcript must add exactly that one touch
	// so push targets re-select the session.
	_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{
		messageCountWrite("session", 4),
	})
	require.NoError(t, err)
	insertTouches := touches()

	resetTouches()
	_, err = d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{
		messageCountWrite("session", 6),
	})
	require.NoError(t, err)
	require.Equal(t, insertTouches+1, touches(),
		"revision bump must touch local_modified_at only for replacements")

	var modifiedAt sql.NullString
	require.NoError(t, d.getReader().QueryRow(t.Context(),
		`SELECT local_modified_at FROM sessions WHERE id = 'session'`,
	).Scan(&modifiedAt), "read local_modified_at")
	require.True(t, modifiedAt.Valid && modifiedAt.String != "",
		"batch-written session must carry local_modified_at")
}

func fillTestSnapshot(t *testing.T, d *DB, query string, args ...any) [][]any {
	t.Helper()
	rows, err := d.rawReader().QueryContext(t.Context(), query, args...)
	require.NoError(t, err)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		out = append(out, vals)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestWriteSessionBatchAtomicFillsEmptyToolResults(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	const id = "fill"
	msg := func(ord int, calls ...ToolCall) Message {
		content := fmt.Sprintf("message-%d", ord)
		return Message{
			SessionID: id, Ordinal: ord, Role: "assistant",
			Content: content, ContentLength: len(content),
			Timestamp:  time.Unix(int64(ord), 0).UTC().Format(time.RFC3339),
			HasToolUse: len(calls) > 0, ToolCalls: calls,
		}
	}
	call := func(name, result string) ToolCall {
		return ToolCall{
			SessionID: id, ToolName: name, Category: "Other",
			ResultContent: result, ResultContentLength: len(result),
		}
	}
	session := Session{
		ID: id, Project: "project", Machine: defaultMachine,
		Agent: "chatgpt", MessageCount: 3,
	}
	_, err := d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{
		Session: session,
		Messages: []Message{
			msg(0, call("a", "")),
			msg(1, call("b", "done"), call("c", "")),
			msg(2, call("d", "")),
		},
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	_, err = d.PinMessage(ctx, id, fillTestMessageID(t, d, id, 1), nil)
	require.NoError(t, err)

	const messagesQuery = `SELECT * FROM messages WHERE session_id = ? ORDER BY ordinal`
	const unfilledCallsQuery = `SELECT tc.* FROM tool_calls tc
		JOIN messages m ON m.id = tc.message_id
		WHERE tc.session_id = ? AND NOT (m.ordinal = 0 AND tc.call_index = 0)
		  AND NOT (m.ordinal = 1 AND tc.call_index = 1)
		ORDER BY m.ordinal, tc.call_index`
	beforeMessages := fillTestSnapshot(t, d, messagesQuery, id)
	beforeCalls := fillTestSnapshot(t, d, unfilledCallsQuery, id)
	require.Len(t, beforeCalls, 2)

	// Two of three archived messages get a fill; the completed call on the
	// second gets a different non-empty candidate, and the empty call there
	// gets a transcripts-shaped candidate with a length and no text.
	session.MessageCount = 4
	transcriptsOnly := call("c", "")
	transcriptsOnly.ResultContentLength = 10
	_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{
		Session: session,
		Messages: []Message{
			msg(0, call("a", "filled")),
			msg(1, call("b", "changed"), transcriptsOnly),
			msg(3),
		},
		FillEmptyToolResults: true,
	}})
	require.NoError(t, err)

	afterMessages := fillTestSnapshot(t, d, messagesQuery, id)
	require.Len(t, afterMessages, 4)
	require.Equal(t, beforeMessages, afterMessages[:3], "no archived message row may change")
	require.Equal(t, beforeCalls, fillTestSnapshot(t, d, unfilledCallsQuery, id))

	msgs, err := d.GetAllMessages(ctx, id)
	require.NoError(t, err)
	require.Len(t, msgs, 4)
	require.Equal(t, "filled", msgs[0].ToolCalls[0].ResultContent)
	require.Equal(t, 6, msgs[0].ToolCalls[0].ResultContentLength)
	require.Equal(t, "done", msgs[1].ToolCalls[0].ResultContent)
	require.Empty(t, msgs[1].ToolCalls[1].ResultContent)
	require.Equal(t, 10, msgs[1].ToolCalls[1].ResultContentLength)
	require.Empty(t, msgs[2].ToolCalls[0].ResultContent)
	require.Equal(t, "message-3", msgs[3].Content)
	pins, err := d.ListPinnedMessages(ctx, id, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	require.Equal(t, msgs[1].ID, pins[0].MessageID)
}

func fillTestMessageID(t *testing.T, d *DB, sessionID string, ordinal int) int64 {
	t.Helper()
	var id int64
	require.NoError(t, d.rawReader().QueryRowContext(t.Context(),
		`SELECT id FROM messages WHERE session_id = ? AND ordinal = ?`,
		sessionID, ordinal,
	).Scan(&id))
	return id
}
