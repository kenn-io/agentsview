package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInsertAndGetMessage_ThinkingText(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	sessionID := "thinking-test"
	insertSession(t, d, sessionID, "proj1")

	insertMessages(t, d, Message{
		SessionID:    sessionID,
		Ordinal:      0,
		Role:         "assistant",
		Content:      "the answer",
		ThinkingText: "I am pondering",
	})

	got, err := d.GetAllMessages(context.Background(), sessionID)
	requireNoError(t, err, "GetAllMessages")
	if len(got) != 1 {
		t.Fatalf("GetAllMessages returned %d messages, want 1", len(got))
	}
	if got[0].ThinkingText != "I am pondering" {
		t.Errorf(
			"ThinkingText = %q, want %q",
			got[0].ThinkingText, "I am pondering",
		)
	}
}

func TestWriteSessionBatchCommitsGoodRowsAndSkipsBadRows(t *testing.T) {
	d := testDB(t)

	requireNoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"INSERT INTO excluded_sessions (id) VALUES (?)",
			"excluded",
		)
		return err
	}), "seed excluded session")
	requireNoError(t, d.UpsertSession(Session{
		ID:      "trashed",
		Project: "proj",
		Machine: defaultMachine,
		Agent:   defaultAgent,
	}), "seed trashed session")
	requireNoError(t, d.SoftDeleteSession("trashed"), "soft delete session")

	health := 95
	grade := "A"
	result, err := d.WriteSessionBatch([]SessionBatchWrite{
		{
			Session: Session{
				ID:               "good",
				Project:          "proj",
				Machine:          defaultMachine,
				Agent:            defaultAgent,
				FirstMessage:     Ptr("hello"),
				MessageCount:     2,
				UserMessageCount: 1,
			},
			Messages: []Message{
				userMsg("good", 0, "hello"),
				{
					SessionID:     "good",
					Ordinal:       1,
					Role:          "assistant",
					Content:       "answer",
					ContentLength: 6,
					ToolCalls: []ToolCall{{
						ToolName:  "Read",
						Category:  "Read",
						ToolUseID: "toolu_1",
					}},
				},
			},
			Signals: SessionSignalUpdate{
				Outcome:           "success",
				OutcomeConfidence: "high",
				EndedWithRole:     "assistant",
				HealthScore:       &health,
				HealthGrade:       &grade,
				HasToolCalls:      true,
			},
			DataVersion: CurrentDataVersion(),
		},
		{
			Session: Session{
				ID:               "bad",
				Project:          "proj",
				Machine:          defaultMachine,
				Agent:            defaultAgent,
				MessageCount:     1,
				UserMessageCount: 1,
			},
			Messages: []Message{
				userMsg("missing-session", 0, "broken"),
			},
			DataVersion: CurrentDataVersion(),
		},
		{
			Session: Session{
				ID:               "excluded",
				Project:          "proj",
				Machine:          defaultMachine,
				Agent:            defaultAgent,
				MessageCount:     1,
				UserMessageCount: 1,
			},
			Messages: []Message{
				userMsg("excluded", 0, "deleted"),
			},
			DataVersion: CurrentDataVersion(),
		},
		{
			Session: Session{
				ID:               "trashed",
				Project:          "proj",
				Machine:          defaultMachine,
				Agent:            defaultAgent,
				MessageCount:     1,
				UserMessageCount: 1,
			},
			Messages: []Message{
				userMsg("trashed", 0, "trashed"),
			},
			DataVersion: CurrentDataVersion(),
		},
	})
	requireNoError(t, err, "WriteSessionBatch")
	if result.WrittenSessions != 1 {
		t.Fatalf("WrittenSessions = %d, want 1", result.WrittenSessions)
	}
	if result.WrittenMessages != 2 {
		t.Fatalf("WrittenMessages = %d, want 2", result.WrittenMessages)
	}
	if result.FailedSessions != 1 {
		t.Fatalf("FailedSessions = %d, want 1", result.FailedSessions)
	}
	if result.ExcludedSessions != 2 {
		t.Fatalf("ExcludedSessions = %d, want 2", result.ExcludedSessions)
	}

	sess, err := d.GetSessionFull(context.Background(), "good")
	requireNoError(t, err, "GetSessionFull good")
	if sess == nil {
		t.Fatal("good session not found")
	}
	if sess.DataVersion != CurrentDataVersion() {
		t.Errorf(
			"DataVersion = %d, want %d",
			sess.DataVersion, CurrentDataVersion(),
		)
	}
	if sess.Outcome != "success" || sess.OutcomeConfidence != "high" {
		t.Errorf(
			"signals = %q/%q, want success/high",
			sess.Outcome, sess.OutcomeConfidence,
		)
	}
	if !sess.HasToolCalls {
		t.Error("HasToolCalls = false, want true")
	}
	trashed, err := d.GetSessionFull(context.Background(), "trashed")
	requireNoError(t, err, "GetSessionFull trashed")
	if trashed == nil || trashed.DeletedAt == nil {
		t.Fatal("trashed session was not preserved in trash")
	}

	msgs, err := d.GetAllMessages(context.Background(), "good")
	requireNoError(t, err, "GetAllMessages good")
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if len(msgs[1].ToolCalls) != 1 {
		t.Fatalf(
			"assistant tool calls = %d, want 1",
			len(msgs[1].ToolCalls),
		)
	}

	bad, err := d.GetSessionFull(context.Background(), "bad")
	requireNoError(t, err, "GetSessionFull bad")
	if bad != nil {
		t.Fatal("bad session should have rolled back")
	}
	excluded, err := d.GetSessionFull(context.Background(), "excluded")
	requireNoError(t, err, "GetSessionFull excluded")
	if excluded != nil {
		t.Fatal("excluded session should not be written")
	}
}

func TestMigration_ThinkingTextColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Create a DB with the current schema then drop the
	// thinking_text column to simulate a pre-migration DB.
	d, err := Open(path)
	requireNoError(t, err, "initial open")
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d,
		userMsg("s1", 0, "hello"),
		Message{
			SessionID:    "s1",
			Ordinal:      1,
			Role:         "assistant",
			Content:      "answer",
			ThinkingText: "pre-migration thought",
		},
	)
	d.Close()

	// Remove thinking_text via ALTER TABLE DROP COLUMN
	// (SQLite 3.35+) to simulate a legacy schema.
	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open")
	_, err = conn.Exec(
		`ALTER TABLE messages DROP COLUMN thinking_text`,
	)
	requireNoError(t, err, "drop thinking_text column")

	// Verify column is gone.
	var count int
	err = conn.QueryRow(
		`SELECT count(*) FROM pragma_table_info('messages')` +
			` WHERE name = 'thinking_text'`,
	).Scan(&count)
	requireNoError(t, err, "verify column removed")
	if count != 0 {
		t.Fatal("expected thinking_text column to be absent")
	}

	// Insert a legacy row with an explicit column list that
	// cannot reference thinking_text (column doesn't exist yet).
	_, err = conn.Exec(`
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			has_thinking, has_tool_use, content_length,
			is_system, model, token_usage,
			context_tokens, output_tokens,
			has_context_tokens, has_output_tokens,
			claude_message_id, claude_request_id,
			source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain,
			is_compact_boundary
		) VALUES (
			's1', 2, 'user', 'legacy', '',
			0, 0, 6,
			0, '', '',
			0, 0,
			0, 0,
			'', '',
			'', '', '',
			'', 0,
			0
		)`)
	requireNoError(t, err, "insert legacy row")
	conn.Close()

	// Reopen with Open() — migration should add the column.
	d2, err := Open(path)
	requireNoError(t, err, "reopen after migration")
	defer d2.Close()

	// Verify column exists.
	err = d2.getReader().QueryRow(
		`SELECT count(*) FROM pragma_table_info('messages')` +
			` WHERE name = 'thinking_text'`,
	).Scan(&count)
	requireNoError(t, err, "verify column added")
	if count != 1 {
		t.Fatal("expected thinking_text column after migration")
	}

	// Verify all rows survive and the legacy row defaults to "".
	msgs, err := d2.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "get messages")
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	for _, m := range msgs {
		if m.ThinkingText != "" {
			t.Errorf(
				"ord=%d ThinkingText = %q, want %q (default)",
				m.Ordinal, m.ThinkingText, "",
			)
		}
	}

	// Insert a new message with ThinkingText and verify round-trip.
	insertMessages(t, d2, Message{
		SessionID:    "s1",
		Ordinal:      3,
		Role:         "assistant",
		Content:      "post-migration answer",
		ThinkingText: "x",
	})
	msgs, err = d2.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "get messages after insert")
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if msgs[3].ThinkingText != "x" {
		t.Errorf(
			"ThinkingText = %q, want %q",
			msgs[3].ThinkingText, "x",
		)
	}
}

// TestReplaceSessionMessages_LargeSession is a perf regression test
// for the FTS5 trigger-cascade hang fixed alongside the bulk-delete
// path in ReplaceSessionMessages. Before the fix, deleting a session
// whose messages contained multi-MB content blobs would fan out into
// per-row FTS 'delete' commands, each tokenizing the old content, and
// could stall the writer for minutes on real data. The bulk path
// makes the cost effectively flat regardless of blob size, so this
// test puts a hard 10s ceiling on the full replace cycle for a
// session that mixes 1000 small messages with one ~5MB content blob.
// Skipped under -short since a clean run is well under 1s but CI
// scheduling jitter can push slow paths up.
func TestReplaceSessionMessages_LargeSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf test in -short mode")
	}
	t.Parallel()
	d := testDB(t)
	const sessionID = "perf-large"
	insertSession(t, d, sessionID, "proj")

	const n = 1000
	msgs := make([]Message, 0, n)
	for i := range n {
		msgs = append(msgs, userMsg(sessionID, i, "small"))
	}
	// One ~5MB content blob in the middle of the stream — the
	// pathological case that blew up the per-row FTS delete path.
	big := strings.Repeat("x ", 5*1024*1024/2)
	msgs[n/2] = Message{
		SessionID:     sessionID,
		Ordinal:       n / 2,
		Role:          "assistant",
		Content:       big,
		ContentLength: len(big),
		Timestamp:     tsZero,
	}
	insertMessages(t, d, msgs...)

	// Replace with a different small set so the delete path has to
	// remove all 1000 rows including the 5MB blob.
	repl := make([]Message, 0, 10)
	for i := range 10 {
		repl = append(repl, userMsg(sessionID, i, "after"))
	}
	start := time.Now()
	if err := d.ReplaceSessionMessages(sessionID, repl); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf(
			"ReplaceSessionMessages took %s, want < 10s "+
				"(per-row FTS trigger regression?)",
			elapsed.Round(time.Millisecond),
		)
	}

	got, err := d.GetAllMessages(context.Background(), sessionID)
	requireNoError(t, err, "GetAllMessages after replace")
	if len(got) != len(repl) {
		t.Fatalf(
			"after replace got %d messages, want %d",
			len(got), len(repl),
		)
	}

	// Verify the FTS index was actually scrubbed: count rows in
	// messages_fts that join back to the (now-deleted) original
	// session rows. Should be zero. If the messages_ad trigger
	// restoration failed silently or the bulk-delete INSERT...SELECT
	// got skipped, stale tokens would still resolve here.
	var leaked int
	err = d.getReader().QueryRow(
		`SELECT count(*) FROM messages_fts
		 WHERE messages_fts MATCH 'xxx'`,
	).Scan(&leaked)
	requireNoError(t, err, "fts leak check")
	if leaked != 0 {
		t.Fatalf(
			"FTS still contains %d rows matching 'xxx' from deleted blob",
			leaked,
		)
	}
}
