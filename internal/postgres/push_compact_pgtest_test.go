//go:build pgtest

package postgres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPushCompactComparisons(t *testing.T) {
	ctx := t.Context()
	const id = "compact"
	const schema = "agentsview_compact_test"
	url := testPGURL(t)
	cleanNamedPGSchema(t, url, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	local := testDB(t)
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "machine", MessageCount: 1}))
	body := strings.Repeat("large-payload-λ", 10000)
	messages := []db.Message{{SessionID: id, Ordinal: 0, Role: "assistant", Content: body, ThinkingText: body, HasThinking: true, HasToolUse: true, Timestamp: "2026-10-01T10:00:00.123456789Z", ToolCalls: []db.ToolCall{{ToolName: "Read", Category: "file", ToolUseID: "call", InputJSON: body, ResultContent: body, ResultContentLength: len(body), FilePath: body, ResultEvents: []db.ToolResultEvent{{Content: body + "event", ContentLength: len(body) + 5, Source: "tool_result", Status: "completed"}}}, {ToolName: "Empty"}}}}
	require.NoError(t, local.InsertMessages(ctx, messages))
	syncer, err := New(url, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	counts, err := local.MessageContentFingerprints(ctx, []string{id})
	require.NoError(t, err)
	tx, err := syncer.pg.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	wrongCounts := map[string]db.MessageContentAggregate{id: {Count: 2}}
	skipped, err := readPushSessionMessageComparisons(ctx, tx, []string{id}, wrongCounts)
	require.NoError(t, err)
	assert.Equal(t, 1, skipped.MessageAggregates[id].Count)
	assert.Empty(t, skipped.MessageContentHash)
	assert.Empty(t, skipped.ToolCallFingerprint)
	compact, err := readPushSessionMessageComparisons(ctx, tx, []string{id}, counts)
	require.NoError(t, err)
	localFP, err := localPushMessageFingerprint(ctx, local, id, "", false)
	require.NoError(t, err)
	calls, err := local.ToolCallComparisonFingerprints(ctx, []string{id})
	require.NoError(t, err)
	localFP.ToolCallFP = calls[id]
	assert.True(t, shouldSkipSessionMessages(id, 1, localFP, false, compact))
	for _, fp := range []string{compact.MessageContentHash[id], compact.MessageFlags[id], compact.ToolCallFingerprint[id], compact.ToolResultFingerprint[id]} {
		assert.Less(t, len(fp), 2000)
		assert.NotContains(t, fp, "large-payload")
	}
	for _, test := range []struct {
		name string
		read func() (string, error)
		want string
	}{
		{"content", func() (string, error) { return pgMessageContentHashFingerprint(ctx, tx, id) }, compact.MessageContentHash[id]},
		{"thinking", func() (string, error) { return pgMessageFlagsFingerprint(ctx, tx, id) }, compact.MessageFlags[id]},
		{"calls", func() (string, error) { return pgToolCallFingerprint(ctx, tx, id) }, compact.ToolCallFingerprint[id]},
		{"results", func() (string, error) { return pgToolResultEventFingerprint(ctx, tx, id) }, compact.ToolResultFingerprint[id]},
	} {
		t.Run(test.name, func(t *testing.T) { got, err := test.read(); require.NoError(t, err); assert.Equal(t, test.want, got) })
	}
	for _, change := range []struct{ name, statement string }{
		{"content", `UPDATE messages SET content = 'x' || substring(content from 2) WHERE session_id = $1`},
		{"thinking", `UPDATE messages SET thinking_text = 'x' || substring(thinking_text from 2) WHERE session_id = $1`},
		{"input", `UPDATE tool_calls SET input_json = 'x' || substring(input_json from 2) WHERE session_id = $1 AND call_index = 0`},
		{"summary", `UPDATE tool_calls SET result_content = 'x' || substring(result_content from 2) WHERE session_id = $1 AND call_index = 0`},
		{"path", `UPDATE tool_calls SET file_path = 'x' || substring(file_path from 2) WHERE session_id = $1 AND call_index = 0`},
		{"event", `UPDATE tool_result_events SET content = 'x' || substring(content from 2) WHERE session_id = $1`},
	} {
		t.Run(change.name, func(t *testing.T) {
			_, err := tx.ExecContext(ctx, `SAVEPOINT change`)
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, change.statement, id)
			require.NoError(t, err)
			changed, err := readPushSessionMessageComparisons(ctx, tx, []string{id}, counts)
			require.NoError(t, err)
			assert.False(t, shouldSkipSessionMessages(id, 1, localFP, false, changed))
			_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT change`)
			require.NoError(t, err)
		})
	}
}

func TestPushCompactLegacyTextRewrittenOnce(t *testing.T) {
	ctx := t.Context()
	const schema = "agentsview_compact_legacy_test"
	const id = "legacy"
	url := testPGURL(t)
	cleanNamedPGSchema(t, url, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, url, schema) })
	local := testDB(t)
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: "claude", Project: "sample", Machine: "machine", MessageCount: 1}))
	require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: "clean\x01text"}}))
	syncer, err := New(url, schema, local, "machine", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	result, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	_, err = syncer.pg.ExecContext(ctx, `UPDATE messages SET content = $1 WHERE session_id = $2`, "clean\x01text", id)
	require.NoError(t, err)
	// Call the publication path directly so the dependency marker doesn't
	// suppress the legacy comparison before it reaches PostgreSQL.
	for i := 0; i < 2; i++ {
		tx, err := syncer.pg.BeginTx(ctx, nil)
		require.NoError(t, err)
		count, err := syncer.pushMessages(ctx, tx, id, false, nil, nil)
		require.NoError(t, err)
		if i == 0 {
			assert.Equal(t, 1, count)
		} else {
			assert.Zero(t, count)
		}
		require.NoError(t, tx.Commit())
	}
	var content string
	require.NoError(t, syncer.pg.QueryRowContext(ctx, `SELECT content FROM messages WHERE session_id=$1`, id).Scan(&content))
	assert.Equal(t, "cleantext", content)
}
