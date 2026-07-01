package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const largeSessionPerfCeiling = 10 * time.Second
const crossSessionNeighborCount = 40
const crossSessionToolCallsPerNeighbor = 250
const crossSessionToolCallTotal = crossSessionNeighborCount * crossSessionToolCallsPerNeighbor
const largeSessionFixtureID = "large-session-fixture"
const largeSessionFixtureToken = "ftslargefixture"
const largeSessionNeighborPrefix = "large-session-neighbor"

var (
	largeSessionTemplateBuildMu sync.Mutex

	largeSessionOnlyOnce sync.Once
	largeSessionOnlyDir  string
	largeSessionOnlyPath string

	largeSessionPoisonOnce sync.Once
	largeSessionPoisonDir  string
	largeSessionPoisonPath string
)

func largeSessionMessages(sessionID, blobToken string) []Message {
	const n = 1000
	msgs := make([]Message, 0, n)
	for i := range n {
		msgs = append(msgs, userMsg(sessionID, i, "small"))
	}
	big := strings.Repeat(blobToken+" ", 5*1024*1024/len(blobToken+" "))
	msgs[n/2] = Message{
		SessionID:     sessionID,
		Ordinal:       n / 2,
		Role:          "assistant",
		Content:       big,
		ContentLength: len(big),
		Timestamp:     tsZero,
	}
	return msgs
}

func openLargeSessionFixtureDB(t *testing.T, withFKPoison bool) *DB {
	t.Helper()

	var src string
	if withFKPoison {
		largeSessionPoisonOnce.Do(func() {
			largeSessionPoisonDir, largeSessionPoisonPath =
				buildLargeSessionFixtureTemplate(t, true)
		})
		src = largeSessionPoisonPath
	} else {
		largeSessionOnlyOnce.Do(func() {
			largeSessionOnlyDir, largeSessionOnlyPath =
				buildLargeSessionFixtureTemplate(t, false)
		})
		src = largeSessionOnlyPath
	}

	dst := filepath.Join(t.TempDir(), "test.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		require.NoError(t,
			copyTemplateDBFile(src+suffix, dst+suffix, suffix == ""),
			"copy large-session fixture %q", suffix)
	}
	d, err := OpenPreparedTestDB(dst)
	require.NoError(t, err, "open large-session fixture")
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	return d
}

func buildLargeSessionFixtureTemplate(
	t *testing.T, withFKPoison bool,
) (string, string) {
	t.Helper()
	largeSessionTemplateBuildMu.Lock()
	defer largeSessionTemplateBuildMu.Unlock()

	dir, err := os.MkdirTemp("", "agentsview-large-session-*")
	require.NoError(t, err, "create large-session fixture dir")
	path := filepath.Join(dir, "test.db")
	require.NoError(t, copyTestDBTemplate(path),
		"copy base db template for large-session fixture")

	d, err := OpenPreparedTestDB(path)
	require.NoError(t, err, "open large-session template")
	insertSession(t, d, largeSessionFixtureID, "proj")
	insertMessages(t, d,
		largeSessionMessages(largeSessionFixtureID, largeSessionFixtureToken)...)
	if withFKPoison {
		seedCrossSessionFKGrowth(t, d, largeSessionNeighborPrefix)
		poisonMessagesDeleteTrigger(t, d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), largeSessionPerfCeiling)
	defer cancel()
	require.NoError(t, d.CheckpointWALTruncate(ctx),
		"checkpoint large-session template")
	require.NoError(t, d.Close(), "close large-session template")
	return dir, path
}

func seedCrossSessionFKGrowth(t *testing.T, d *DB, sessionID string) {
	t.Helper()

	type neighborSeed struct {
		sessionID string
		messageID int64
	}

	seeds := make([]neighborSeed, 0, crossSessionNeighborCount)
	for i := range crossSessionNeighborCount {
		neighborID := sessionID + "-" + strconv.Itoa(i)
		insertSession(t, d, neighborID, "proj")
		insertMessages(t, d, userMsg(neighborID, 0, "neighbor"))
		msgs, err := d.GetAllMessages(context.Background(), neighborID)
		require.NoError(t, err, "GetAllMessages neighbor %s", neighborID)
		require.Len(t, msgs, 1)
		_, err = d.PinMessage(neighborID, msgs[0].ID, nil)
		require.NoError(t, err, "PinMessage neighbor %s", neighborID)
		seeds = append(seeds, neighborSeed{
			sessionID: neighborID,
			messageID: msgs[0].ID,
		})
	}

	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		for _, seed := range seeds {
			for i := range crossSessionToolCallsPerNeighbor {
				if _, err := tx.Exec(
					`INSERT INTO tool_calls
					 (message_id, session_id, tool_name, category, tool_use_id)
					 VALUES (?, ?, ?, ?, ?)`,
					seed.messageID, seed.sessionID, "Read", "Read",
					seed.sessionID+"-tool-"+strconv.Itoa(i),
				); err != nil {
					return err
				}
			}
		}
		return nil
	}), "seed neighbor tool_calls")
}

func assertNoFTSLeak(t *testing.T, d *DB, token string) {
	t.Helper()
	var leaked int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM messages_fts
		 WHERE messages_fts MATCH ?`,
		token,
	).Scan(&leaked)
	require.NoError(t, err, "fts leak check")
	assert.Zero(t, leaked, "FTS still contains rows matching %q", token)
}

func poisonMessagesDeleteTrigger(t *testing.T, d *DB) {
	t.Helper()

	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		if _, err := tx.Exec("DROP TRIGGER IF EXISTS messages_ad"); err != nil {
			return err
		}
		_, err := tx.Exec(`
			CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
				SELECT RAISE(FAIL, 'poison messages_ad fired');
			END`)
		return err
	}), "poison messages_ad trigger")
}

func requireMessagesDeleteTriggerRestored(t *testing.T, d *DB) {
	t.Helper()

	var triggerSQL string
	err := d.getReader().QueryRow(
		`SELECT sql FROM sqlite_master
		 WHERE type = 'trigger' AND name = 'messages_ad'`,
	).Scan(&triggerSQL)
	require.NoError(t, err, "read messages_ad trigger")
	assert.NotContains(t, triggerSQL, "poison messages_ad fired",
		"messages_ad trigger was not restored")
	assert.Contains(t, triggerSQL, "INSERT INTO messages_fts",
		"messages_ad trigger no longer matches the canonical FTS delete path")
}

func requireMessagesDeleteTriggerPoisoned(t *testing.T, d *DB) {
	t.Helper()

	var triggerSQL string
	err := d.getReader().QueryRow(
		`SELECT sql FROM sqlite_master
		 WHERE type = 'trigger' AND name = 'messages_ad'`,
	).Scan(&triggerSQL)
	require.NoError(t, err, "read messages_ad trigger")
	assert.Contains(t, triggerSQL, "poison messages_ad fired",
		"fresh batch write should not touch the delete trigger")
}

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
	require.NoError(t, err, "GetAllMessages")
	require.Len(t, got, 1)
	assert.Equal(t, "I am pondering", got[0].ThinkingText, "ThinkingText")
}

func TestWriteSessionBatchCommitsGoodRowsAndSkipsBadRows(t *testing.T) {
	d := testDB(t)

	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"INSERT INTO excluded_sessions (id) VALUES (?)",
			"excluded",
		)
		return err
	}), "seed excluded session")
	require.NoError(t, d.UpsertSession(Session{
		ID:      "trashed",
		Project: "proj",
		Machine: defaultMachine,
		Agent:   defaultAgent,
	}), "seed trashed session")
	require.NoError(t, d.SoftDeleteSession("trashed"), "soft delete session")

	health := 95
	grade := "A"
	result, err := d.WriteSessionBatch([]SessionBatchWrite{
		{
			Session: Session{
				ID:               "good",
				Project:          "proj",
				Machine:          defaultMachine,
				Agent:            defaultAgent,
				FirstMessage:     new(string("hello")),
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
	require.NoError(t, err, "WriteSessionBatch")
	require.Equal(t, 1, result.WrittenSessions, "WrittenSessions")
	require.Equal(t, 2, result.WrittenMessages, "WrittenMessages")
	require.Equal(t, 1, result.FailedSessions, "FailedSessions")
	require.Equal(t, 2, result.ExcludedSessions, "ExcludedSessions")

	sess, err := d.GetSessionFull(context.Background(), "good")
	require.NoError(t, err, "GetSessionFull good")
	require.NotNil(t, sess, "good session not found")
	assert.Equal(t, CurrentDataVersion(), sess.DataVersion, "DataVersion")
	assert.Equal(t, "success", sess.Outcome, "Outcome")
	assert.Equal(t, "high", sess.OutcomeConfidence, "OutcomeConfidence")
	assert.True(t, sess.HasToolCalls, "HasToolCalls")
	trashed, err := d.GetSessionFull(context.Background(), "trashed")
	require.NoError(t, err, "GetSessionFull trashed")
	require.NotNil(t, trashed, "trashed session was not preserved in trash")
	assert.NotNil(t, trashed.DeletedAt, "trashed session was not preserved in trash")

	msgs, err := d.GetAllMessages(context.Background(), "good")
	require.NoError(t, err, "GetAllMessages good")
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolCalls, 1, "assistant tool calls")

	bad, err := d.GetSessionFull(context.Background(), "bad")
	require.NoError(t, err, "GetSessionFull bad")
	assert.Nil(t, bad, "bad session should have rolled back")
	excluded, err := d.GetSessionFull(context.Background(), "excluded")
	require.NoError(t, err, "GetSessionFull excluded")
	assert.Nil(t, excluded, "excluded session should not be written")
}

func TestMigration_ThinkingTextColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Create a DB with the current schema then drop the
	// thinking_text column to simulate a pre-migration DB.
	d, err := Open(path)
	require.NoError(t, err, "initial open")
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
	require.NoError(t, err, "raw open")
	_, err = conn.Exec(
		`ALTER TABLE messages DROP COLUMN thinking_text`,
	)
	require.NoError(t, err, "drop thinking_text column")

	// Verify column is gone.
	var count int
	err = conn.QueryRow(
		`SELECT count(*) FROM pragma_table_info('messages')` +
			` WHERE name = 'thinking_text'`,
	).Scan(&count)
	require.NoError(t, err, "verify column removed")
	require.Zero(t, count, "expected thinking_text column to be absent")

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
	require.NoError(t, err, "insert legacy row")
	conn.Close()

	// Reopen with Open() — migration should add the column.
	d2, err := Open(path)
	require.NoError(t, err, "reopen after migration")
	defer d2.Close()

	// Verify column exists.
	err = d2.getReader().QueryRow(
		`SELECT count(*) FROM pragma_table_info('messages')` +
			` WHERE name = 'thinking_text'`,
	).Scan(&count)
	require.NoError(t, err, "verify column added")
	require.Equal(t, 1, count, "expected thinking_text column after migration")

	// Verify all rows survive and the legacy row defaults to "".
	msgs, err := d2.GetAllMessages(context.Background(), "s1")
	require.NoError(t, err, "get messages")
	require.Len(t, msgs, 3)
	for _, m := range msgs {
		assert.Empty(t, m.ThinkingText, "ord=%d ThinkingText", m.Ordinal)
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
	require.NoError(t, err, "get messages after insert")
	require.Len(t, msgs, 4)
	assert.Equal(t, "x", msgs[3].ThinkingText, "ThinkingText")
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
	d := openLargeSessionFixtureDB(t, false)
	requireFTS(t, d)

	// Replace with a different small set so the delete path has to
	// remove all 1000 rows including the 5MB blob.
	repl := make([]Message, 0, 10)
	for i := range 10 {
		repl = append(repl, userMsg(largeSessionFixtureID, i, "after"))
	}
	start := time.Now()
	require.NoError(t, d.ReplaceSessionMessages(largeSessionFixtureID, repl),
		"ReplaceSessionMessages")
	elapsed := time.Since(start)
	require.LessOrEqual(t, elapsed, largeSessionPerfCeiling,
		"ReplaceSessionMessages took %s, want < 10s (per-row FTS trigger regression?)",
		elapsed.Round(time.Millisecond))

	got, err := d.GetAllMessages(context.Background(), largeSessionFixtureID)
	require.NoError(t, err, "GetAllMessages after replace")
	require.Len(t, got, len(repl), "after replace")

	// Verify the FTS index was actually scrubbed: count rows in
	// messages_fts that join back to the (now-deleted) original
	// session rows. Should be zero. If the messages_ad trigger
	// restoration failed silently or the bulk-delete INSERT...SELECT
	// got skipped, stale tokens would still resolve here.
	assertNoFTSLeak(t, d, largeSessionFixtureToken)
}

func TestWriteSessionBatch_ReplaceMessagesLargeSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf test in -short mode")
	}
	t.Parallel()
	d := openLargeSessionFixtureDB(t, true)
	requireFTS(t, d)

	repl := make([]Message, 0, 10)
	for i := range 10 {
		repl = append(repl, userMsg(largeSessionFixtureID, i, "after"))
	}
	start := time.Now()
	result, err := d.WriteSessionBatch([]SessionBatchWrite{{
		Session: Session{
			ID:               largeSessionFixtureID,
			Project:          "proj",
			Machine:          defaultMachine,
			Agent:            defaultAgent,
			MessageCount:     len(repl),
			UserMessageCount: len(repl),
		},
		Messages:        repl,
		DataVersion:     CurrentDataVersion(),
		ReplaceMessages: true,
	}})
	require.NoError(t, err, "WriteSessionBatch")
	elapsed := time.Since(start)
	require.Equal(t, 1, result.WrittenSessions, "WrittenSessions")
	require.Equal(t, len(repl), result.WrittenMessages, "WrittenMessages")
	require.LessOrEqual(t, elapsed, largeSessionPerfCeiling,
		"WriteSessionBatch replace took %s, want < 10s (per-row FTS trigger regression?)",
		elapsed.Round(time.Millisecond))

	got, err := d.GetAllMessages(context.Background(), largeSessionFixtureID)
	require.NoError(t, err, "GetAllMessages after batch replace")
	require.Len(t, got, len(repl), "after batch replace")
	assertNoFTSLeak(t, d, largeSessionFixtureToken)
	requireMessagesDeleteTriggerRestored(t, d)

	var neighborToolCalls int
	err = d.getReader().QueryRow(
		"SELECT count(*) FROM tool_calls WHERE session_id LIKE ?",
		largeSessionNeighborPrefix+"-%",
	).Scan(&neighborToolCalls)
	require.NoError(t, err, "neighbor tool_calls count")
	assert.Equal(t, crossSessionToolCallTotal, neighborToolCalls,
		"neighbor tool_calls count")
}

func TestWriteSessionBatchFreshReplaceMessagesSkipsDeletePath(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	requireFTS(t, d)
	poisonMessagesDeleteTrigger(t, d)

	const sessionID = "batch-fresh"
	result, err := d.WriteSessionBatch([]SessionBatchWrite{{
		Session: Session{
			ID:               sessionID,
			Project:          "proj",
			Machine:          defaultMachine,
			Agent:            defaultAgent,
			MessageCount:     1,
			UserMessageCount: 1,
		},
		Messages: []Message{
			userMsg(sessionID, 0, "fresh"),
		},
		DataVersion:     CurrentDataVersion(),
		ReplaceMessages: true,
	}})
	require.NoError(t, err, "WriteSessionBatch")
	assert.Equal(t, 1, result.WrittenSessions, "WrittenSessions")
	assert.Equal(t, 1, result.WrittenMessages, "WrittenMessages")
	requireMessagesDeleteTriggerPoisoned(t, d)
}

// TestMessageReadsTolerateNullTimestamp pins NULL-timestamp robustness
// across the three message read paths. timestamp is the only nullable
// text column in the messages table; fresh inserts always bind a Go
// string (never NULL), so a NULL only reaches a row via an imported or
// migrated archive. Before the COALESCE guard such a row made rows.Scan
// fail with "converting NULL to string is unsupported", which aborted
// the parse-diff run (MessageRoleTimeFingerprint) and broke ordinary
// reads (GetAllMessages, GetMessageByOrdinal).
func TestMessageReadsTolerateNullTimestamp(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	insertSession(t, d, "null-ts", "proj1")
	insertMessages(t, d,
		userMsgAt("null-ts", 0, "hello", "2024-01-01T10:00:00Z"),
		asstMsgAt("null-ts", 1, "hi there", "2024-01-01T10:00:05Z"),
	)

	nullOrdinal1 := func() {
		require.NoError(t, d.Update(func(tx *sql.Tx) error {
			_, err := tx.Exec(
				"UPDATE messages SET timestamp = NULL"+
					" WHERE session_id = ? AND ordinal = ?", "null-ts", 1)
			return err
		}), "null the stored timestamp")
	}

	// Tier-1 fingerprint: must not error, and a NULL must fingerprint
	// identically to an empty string so the report cannot distinguish
	// the two. InsertMessages binds a Go string, so reach past it with
	// raw SQL to plant the NULL.
	nullOrdinal1()
	fpNull, err := d.MessageRoleTimeFingerprint("null-ts")
	require.NoError(t, err, "fingerprint over NULL timestamp")
	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE messages SET timestamp = ''"+
				" WHERE session_id = ? AND ordinal = ?", "null-ts", 1)
		return err
	}), "set the stored timestamp to empty string")
	fpEmpty, err := d.MessageRoleTimeFingerprint("null-ts")
	require.NoError(t, err, "fingerprint over empty timestamp")
	assert.Equal(t, fpEmpty, fpNull,
		"NULL timestamp must fingerprint identically to empty string")

	// Tier-2 batch read path (scanMessages via selectMessageCols).
	nullOrdinal1()
	msgs, err := d.GetAllMessages(context.Background(), "null-ts")
	require.NoError(t, err, "GetAllMessages over NULL timestamp")
	require.Len(t, msgs, 2)
	assert.Equal(t, "2024-01-01T10:00:00Z", msgs[0].Timestamp)
	assert.Equal(t, "", msgs[1].Timestamp,
		"NULL timestamp reads as empty string")

	// Single-row read path (GetMessageByOrdinal via selectMessageCols).
	m, err := d.GetMessageByOrdinal("null-ts", 1)
	require.NoError(t, err, "GetMessageByOrdinal over NULL timestamp")
	require.NotNil(t, m)
	assert.Equal(t, "", m.Timestamp,
		"NULL timestamp reads as empty string")
}

func TestToolCallFilePathCallIndexRoundTrip(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "sess-1", "proj")
	insertMessages(t, d, userMsg("sess-1", 0, "hello"))

	// Fetch the message id assigned by the DB.
	var msgID int64
	require.NoError(t, d.getReader().QueryRowContext(ctx,
		`SELECT id FROM messages WHERE session_id = 'sess-1' AND ordinal = 0`,
	).Scan(&msgID))

	tx, err := d.getWriter().Begin()
	require.NoError(t, err, "begin tx")
	err = insertToolCallsChunkTx(tx, []ToolCall{
		{
			MessageID: msgID, SessionID: "sess-1",
			ToolName: "Edit", Category: "Edit",
			ToolUseID: "tu1", InputJSON: `{"file_path":"/a/b.go"}`,
			FilePath: "/a/b.go", CallIndex: 0,
		},
		{
			MessageID: msgID, SessionID: "sess-1",
			ToolName: "Write", Category: "Write",
			ToolUseID: "tu2", InputJSON: `{"file":"/c/d.go"}`,
			FilePath: "/c/d.go", CallIndex: 1,
		},
	})
	require.NoError(t, err, "insertToolCallsChunkTx")
	require.NoError(t, tx.Commit(), "commit")

	var fp string
	var ci int
	require.NoError(t, d.getReader().QueryRowContext(ctx,
		`SELECT file_path, call_index FROM tool_calls
		 WHERE tool_use_id = 'tu2'`).Scan(&fp, &ci))
	assert.Equal(t, "/c/d.go", fp)
	assert.Equal(t, 1, ci)
}
