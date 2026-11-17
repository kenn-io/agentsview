//go:build chtest

package clickhouse

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// A session that only grew keeps its stored message rows; the push writes the
// new message alone and keeps the content version of the rows it kept.
func TestPushAppendsMessagesToGrownSession(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, s, conn := changedOnlyFixture(t)
	store := newTestStore(t, conn, s.target)
	usageCarryPublished := func() bool {
		snapshotCtx, err := store.withPartsSnapshot(ctx)
		require.NoError(t, err)
		return store.usageCarryPublished(snapshotCtx)
	}
	require.True(t, usageCarryPublished())
	before := readPushVersions(t, conn, fixtureBetaID)
	msgs, err := local.GetAllMessages(ctx, fixtureBetaID)
	require.NoError(t, err)

	growSession(t, local, fixtureBetaID, "beta second", "2026-01-11T00:10:00.000Z")
	grown, err := local.GetAllMessages(ctx, fixtureBetaID)
	require.NoError(t, err)
	require.Len(t, grown, 2)
	require.Equal(t, msgs[0].ID, grown[0].ID, "the archive keeps the earlier message's row")

	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsPushed)
	assert.Equal(t, 1, res.SessionsAppended)
	assert.Equal(t, 1, res.MessagesPushed, "only the new message is written")

	after := readPushVersions(t, conn, fixtureBetaID)
	assert.Greater(t, after.published, before.published)
	assert.Equal(t, before.content, after.content, "the session keeps the version of its oldest rows")
	assert.Equal(t, []uint64{before.messages[0], after.published}, after.messages)

	var usageRows int
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT count() FROM usage_messages m JOIN sessions s ON s.id = m.session_id WHERE s.id = ? AND "+
			chUsageMessageCurrent, fixtureBetaID).Scan(&usageRows))
	assert.Equal(t, 2, usageRows, "usage reads see the kept and the appended message")
	assert.True(t, usageCarryPublished(), "an appended session counts as fully published")

	binding, err := store.SessionSourceBinding(ctx, fixtureBetaID)
	require.NoError(t, err, "kept and appended rows both belong to the published session")
	assert.NotEmpty(t, binding)
	stored, err := store.GetAllMessages(ctx, fixtureBetaID)
	require.NoError(t, err)
	require.Len(t, stored, 2)
	assert.Equal(t, "beta second", stored[1].Content)

	again, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, again.SessionsPushed, "the appended session row records the current fingerprints")
}

// Tool calls, result events, and terminal event snapshots stay readable when
// a push appends to a session whose earlier messages carry them.
func TestPushAppendKeepsToolRowsOfEarlierMessages(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, s, conn := changedOnlyFixture(t)
	var lastTerminalBefore sql.NullTime
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT last_terminal_at FROM terminal_event_snapshots WHERE session_id = ?", fixtureAlphaID,
	).Scan(&lastTerminalBefore))
	before := readPushVersions(t, conn, fixtureAlphaID)

	growSession(t, local, fixtureAlphaID, "alpha third", "2026-01-10T00:10:00.000Z")
	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsAppended)
	assert.Equal(t, 1, res.MessagesPushed)

	after := readPushVersions(t, conn, fixtureAlphaID)
	assert.Equal(t, before.content, after.content)
	assert.Equal(t, slices.Concat(before.messages, []uint64{after.published}), after.messages)
	var toolCalls, results int
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT count() FROM tool_calls WHERE session_id = ?", fixtureAlphaID).Scan(&toolCalls))
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT count() FROM tool_result_events WHERE session_id = ?", fixtureAlphaID).Scan(&results))
	assert.Equal(t, 2, toolCalls)
	assert.Equal(t, 1, results)

	var terminalVersion uint64
	var lastTerminalAfter sql.NullTime
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT push_version, last_terminal_at FROM terminal_event_snapshots WHERE session_id = ?", fixtureAlphaID,
	).Scan(&terminalVersion, &lastTerminalAfter))
	assert.Equal(t, after.published, terminalVersion)
	assert.Equal(t, lastTerminalBefore, lastTerminalAfter, "the kept result event still counts")

	store := newTestStore(t, conn, s.target)
	_, err = store.SessionSourceBinding(ctx, fixtureAlphaID)
	require.NoError(t, err)
}

// A change to an earlier message rewrites the whole session even when the
// session also grew.
func TestPushRewritesSessionWhenEarlierMessageChanged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, s, conn := changedOnlyFixture(t)
	sess, err := local.GetSessionFull(ctx, fixtureBetaID)
	require.NoError(t, err)
	msgs, err := local.GetAllMessages(ctx, fixtureBetaID)
	require.NoError(t, err)
	msgs[0].Content = "beta first, edited"
	msgs[0].ContentLength = len(msgs[0].Content)
	msgs = append(msgs, fixtureMessage(fixtureBetaID, 1, "assistant", "beta second", "2026-01-11T00:10:00.000Z"))
	sess.MessageCount = len(msgs)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: *sess, Messages: msgs, DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)

	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, res.SessionsAppended)
	assert.Equal(t, 2, res.MessagesPushed)
	after := readPushVersions(t, conn, fixtureBetaID)
	assert.Equal(t, after.published, after.content)
	assert.Equal(t, []uint64{after.content, after.content}, after.messages)
	var content string
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT content FROM messages WHERE session_id = ? AND ordinal = 0", fixtureBetaID).Scan(&content))
	assert.Equal(t, "beta first, edited", content)
}

// Rows an interrupted push left after the stored messages are not part of the
// session; the next append removes the ones it does not replace.
func TestPushAppendRemovesRowsLeftAfterStoredMessages(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, s, conn := changedOnlyFixture(t)
	before := readPushVersions(t, conn, fixtureBetaID)
	// An unpublished push wrote ordinals 1 and 5 before it stopped.
	for _, ordinal := range []int{1, 5} {
		_, err := conn.ExecContext(ctx, `INSERT INTO messages SELECT * REPLACE (
				? AS ordinal, 'left behind' AS content, push_version + 1 AS push_version)
			FROM (SELECT * FROM messages WHERE session_id = ? AND ordinal = 0)`, ordinal, fixtureBetaID)
		require.NoError(t, err)
	}

	growSession(t, local, fixtureBetaID, "beta second", "2026-01-11T00:10:00.000Z")
	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsAppended)

	after := readPushVersions(t, conn, fixtureBetaID)
	assert.Equal(t, before.content, after.content)
	rows, err := conn.QueryContext(ctx,
		"SELECT ordinal, content FROM messages WHERE session_id = ? ORDER BY ordinal", fixtureBetaID)
	require.NoError(t, err)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var ordinal int
		var content string
		require.NoError(t, rows.Scan(&ordinal, &content))
		got = append(got, content)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"beta first", "beta second"}, got)
}

// A session pushed while its last message waits on a tool result keeps the
// rows of its earlier messages when the result arrives: the push writes that
// message again with the new ones.
func TestPushAppendRewritesLastStoredMessageWhenItsToolResultArrives(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	local, s, conn := changedOnlyFixture(t)
	original := readPushVersions(t, conn, fixtureBetaID)
	sess, err := local.GetSessionFull(ctx, fixtureBetaID)
	require.NoError(t, err)
	const askedAt, answeredAt = "2026-01-11T00:10:00.000Z", "2026-01-11T00:11:00.000Z"
	call := db.ToolCall{ToolName: "Bash", Category: "Bash", ToolUseID: "late-result", InputJSON: `{"command":"make"}`}
	asked := fixtureMessage(fixtureBetaID, 1, "assistant", "running make", askedAt, call)
	sess.MessageCount, sess.EndedAt, sess.LocalModifiedAt = 2, new(askedAt), new(askedAt)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: *sess, Messages: []db.Message{asked}, DataVersion: 1}})
	require.NoError(t, err)
	res, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.SessionsAppended)
	waiting := readPushVersions(t, conn, fixtureBetaID)

	call.ResultContent, call.ResultContentLength = "build ok", len("build ok")
	answered := fixtureMessage(fixtureBetaID, 1, "assistant", "running make", askedAt, call)
	next := fixtureMessage(fixtureBetaID, 2, "assistant", "make passed", answeredAt)
	sess.MessageCount, sess.EndedAt, sess.LocalModifiedAt = 3, new(answeredAt), new(answeredAt)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: *sess, Messages: []db.Message{answered, next}, DataVersion: 1, CompleteStoredRows: true,
	}})
	require.NoError(t, err)
	res, err = s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.SessionsAppended)
	assert.Equal(t, 2, res.MessagesPushed, "the waiting message is written again with the new one")

	after := readPushVersions(t, conn, fixtureBetaID)
	assert.Equal(t, original.content, after.content)
	assert.Equal(t, []uint64{waiting.messages[0], after.published, after.published}, after.messages)
	var resultLength int
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT result_content_length FROM tool_calls WHERE session_id = ? AND message_ordinal = 1",
		fixtureBetaID).Scan(&resultLength))
	assert.Equal(t, len("build ok"), resultLength)

	again, err := s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Zero(t, again.SessionsPushed)
}
