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
		CompleteStoredRows: true,
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

func extendTestSession(id string, count int) Session {
	return Session{
		ID: id, Project: "project", Machine: defaultMachine,
		Agent: "chatgpt", MessageCount: count,
	}
}

func extendTestMsg(id string, ord int, content string) Message {
	return Message{
		SessionID: id, Ordinal: ord, Role: "assistant",
		Content: content, ContentLength: len(content),
		Timestamp: time.Unix(int64(ord), 0).UTC().Format(time.RFC3339),
	}
}

func extendTestRevision(t *testing.T, d *DB, id string) string {
	t.Helper()
	session, err := d.GetSession(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.TranscriptRevision)
	return *session.TranscriptRevision
}

func TestWriteSessionBatchAtomicExtendsTruncatedText(t *testing.T) {
	const messagesQuery = `SELECT * FROM messages WHERE session_id = ? ORDER BY ordinal`
	const untouchedQuery = `SELECT * FROM messages WHERE session_id = ? AND ordinal != 0 ORDER BY ordinal`
	stored := []string{"cut short", "kept text", "", "same text"}
	candidates := []string{"cut short and finished with quillword", "other text", "filled in", "same text"}
	seed := func(t *testing.T, d *DB, id string) {
		t.Helper()
		msgs := make([]Message, len(stored))
		for i, c := range stored {
			msgs[i] = extendTestMsg(id, i, c)
		}
		_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{
			Session: extendTestSession(id, len(msgs)), Messages: msgs, ReplaceMessages: true,
		}})
		require.NoError(t, err)
	}
	candidateWrite := func(id string, complete bool) SessionBatchWrite {
		msgs := make([]Message, 0, len(candidates)+1)
		for i, c := range candidates {
			msgs = append(msgs, extendTestMsg(id, i, c))
		}
		msgs = append(msgs, extendTestMsg(id, len(candidates), "appended reply"))
		return SessionBatchWrite{
			Session:            extendTestSession(id, len(msgs)),
			Messages:           msgs,
			CompleteStoredRows: complete,
		}
	}

	t.Run("extends only strict prefixes", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		const id = "extend"
		seed(t, d, id)
		before, err := d.GetAllMessages(ctx, id)
		require.NoError(t, err)
		_, err = d.PinMessage(ctx, id, before[0].ID, nil)
		require.NoError(t, err)
		untouched := fillTestSnapshot(t, d, untouchedQuery, id)
		revBefore := extendTestRevision(t, d, id)

		_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{candidateWrite(id, true)})
		require.NoError(t, err)

		after, err := d.GetAllMessages(ctx, id)
		require.NoError(t, err)
		require.Len(t, after, 5)
		for i := range before {
			require.Equal(t, before[i].ID, after[i].ID)
		}
		require.Equal(t, candidates[0], after[0].Content)
		require.Equal(t, len(candidates[0]), after[0].ContentLength)
		require.Equal(t, "appended reply", after[4].Content)
		require.Equal(t, untouched, fillTestSnapshot(t, d, untouchedQuery, id)[:3],
			"non-prefix, empty and equal rows must stay byte-identical")
		require.NotEqual(t, revBefore, extendTestRevision(t, d, id))

		pins, err := d.ListPinnedMessages(ctx, id, "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		require.Equal(t, before[0].ID, pins[0].MessageID)

		results, err := d.SearchContent(ctx, ContentSearchFilter{
			Pattern: "quillword", Sources: []string{"messages"},
			IncludeOneShot: true, IncludeAutomated: true, Limit: 10,
		})
		require.NoError(t, err)
		require.NotEmpty(t, results.Matches)
		require.Equal(t, id, results.Matches[0].SessionID)
	})

	t.Run("without the flag", func(t *testing.T) {
		d := testDB(t)
		const id = "plain"
		seed(t, d, id)
		before := fillTestSnapshot(t, d, messagesQuery, id)

		_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{candidateWrite(id, false)})
		require.NoError(t, err)

		after := fillTestSnapshot(t, d, messagesQuery, id)
		require.Len(t, after, 5)
		require.Equal(t, before, after[:4], "no stored row may change")
	})

	t.Run("revokes recall evidence", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		const id = "recall-extend"
		seedRecallEvidenceWindow(t, d, id, 10, "extend", "")
		insertVerifiedRecallSelection(t, d, "extend-entry", id, 10, 11, []string{"tool-a"})
		require.True(t, requireRecallEntry(t, d, "extend-entry").ProvenanceOK)
		messages, err := d.GetAllMessages(ctx, id)
		require.NoError(t, err)
		require.Len(t, messages, 3)
		messages[0].Content += " Then run the tests."
		messages[0].ContentLength = len(messages[0].Content)
		session, err := d.GetSession(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, session)

		_, err = d.WriteSessionBatchAtomic(ctx, []SessionBatchWrite{{
			Session:            *session,
			Messages:           messages[:1],
			CompleteStoredRows: true,
		}})
		require.NoError(t, err)

		stored, err := d.GetAllMessages(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "Run the formatter. Then run the tests.", stored[0].Content)
		require.False(t, requireRecallEntry(t, d, "extend-entry").ProvenanceOK)
	})
}

func extendTestExportActive(t *testing.T, d *DB) bool {
	t.Helper()
	var active bool
	require.NoError(t, d.rawReader().QueryRowContext(t.Context(),
		`SELECT EXISTS(SELECT 1 FROM archive_metadata WHERE key = ?)`,
		conversationExportInitializedKey,
	).Scan(&active))
	return active
}

func TestWriteSessionBatchAtomicExtendedTextReconcilesConversationExport(t *testing.T) {
	const id = "export-extend"
	const cut = "The reply was cut"
	const full = "The reply was cut short by the converter"
	seed := func(t *testing.T, d *DB) {
		t.Helper()
		_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{
			Session: extendTestSession(id, 2),
			Messages: []Message{
				{SessionID: id, Ordinal: 0, Role: "user", Content: "Question", ContentLength: 8},
				extendTestMsg(id, 1, cut),
			},
			ReplaceMessages: true,
		}})
		require.NoError(t, err)
	}
	repair := func(t *testing.T, d *DB) {
		t.Helper()
		_, err := d.WriteSessionBatchAtomic(t.Context(), []SessionBatchWrite{{
			Session:            extendTestSession(id, 2),
			Messages:           []Message{extendTestMsg(id, 1, full)},
			CompleteStoredRows: true,
		}})
		require.NoError(t, err)
	}
	body := func(t *testing.T, d *DB, page ConversationExportResult, change ConversationChange) string {
		t.Helper()
		msg, err := d.GetConversationMessage(t.Context(), ConversationMessageOptions{
			DatabaseID: page.DatabaseID, SessionID: id, MessageID: change.MessageID, Revision: change.Revision,
		})
		require.NoError(t, err)
		require.NotNil(t, msg.Text)
		return *msg.Text
	}

	t.Run("active export keeps message id", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		seed(t, d)
		initial, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
		require.NoError(t, err)
		var original ConversationChange
		for _, change := range initial.Changes {
			if change.Type == "message" && change.Ordinal == 1 {
				original = change
			}
		}
		require.NotEmpty(t, original.MessageID)
		require.Equal(t, cut, body(t, d, initial, original))

		repair(t, d)

		delta, err := d.ExportConversationChanges(ctx, ConversationExportOptions{Checkpoint: initial.Checkpoint})
		require.NoError(t, err)
		var repaired []ConversationChange
		for _, change := range delta.Changes {
			if change.Type == "message" {
				repaired = append(repaired, change)
			}
		}
		require.Len(t, repaired, 1)
		require.Equal(t, original.MessageID, repaired[0].MessageID)
		require.Equal(t, 1, repaired[0].Ordinal)
		require.False(t, repaired[0].Deleted)
		require.NotEqual(t, original.Revision, repaired[0].Revision)
		require.Equal(t, int64(len(full)), repaired[0].TextBytes)
		require.Equal(t, full, body(t, d, delta, repaired[0]))
	})

	t.Run("cold export stays cold", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		seed(t, d)
		require.False(t, extendTestExportActive(t, d))

		repair(t, d)

		require.False(t, extendTestExportActive(t, d), "a repair must not initialize the export")
		page, err := d.ExportConversationChanges(ctx, ConversationExportOptions{})
		require.NoError(t, err)
		var found bool
		for _, change := range page.Changes {
			if change.Type == "message" && change.Ordinal == 1 {
				found = true
				require.Equal(t, full, body(t, d, page, change))
			}
		}
		require.True(t, found)
	})
}
