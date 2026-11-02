package parser

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// claudeHookImportRows returns the hook rows of a message slice.
func claudeHookImportRows(msgs []ParsedMessage) []ParsedMessage {
	var rows []ParsedMessage
	for _, m := range msgs {
		if m.SourceSubtype == "hook_success" {
			rows = append(rows, m)
		}
	}
	return rows
}

// A content-bearing hook_success attachment is merged into the
// transcript as a system row whose body names the hook, the hook event,
// and the recorded content, with the recorded text kept verbatim.
func TestClaudeHookImportRendersHookBodyVerbatim(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-01-01T00:00:00Z"),
		`{"type":"attachment","timestamp":"2026-01-01T00:00:01Z",`+
			`"uuid":"hook-row","parentUuid":"native-parent",`+
			`"isSidechain":true,"promptSource":"synthetic-origin",`+
			`"attachment":{"type":"hook_success","hookName":"example-hook",`+
			`"hookEvent":"PostToolUse","content":"hook finished"}}`,
		testjsonl.ClaudeAssistantJSON("hi", "2026-01-01T00:00:02Z"),
	)

	sess, msgs := runClaudeParserTest(t, "hook-body.jsonl", content)
	require.Len(t, msgs, 3)

	hook := msgs[1]
	assert.Equal(t, RoleUser, hook.Role)
	assert.True(t, hook.IsSystem)
	assert.Equal(t, "attachment", hook.SourceType)
	assert.Equal(t, "hook_success", hook.SourceSubtype)
	assert.Equal(t, "hook-row", hook.SourceUUID)
	assert.Equal(t, "native-parent", hook.SourceParentUUID)
	assert.True(t, hook.IsSidechain)
	assert.Equal(t, "synthetic-origin", hook.PromptSource)
	assert.Equal(t,
		"Hook name: example-hook\n\nHook event: PostToolUse\n\n"+
			"Content:\nhook finished",
		hook.Content)
	assert.Equal(t, len(hook.Content), hook.ContentLength)
	require.NotNil(t, hook.DialogueText)
	assert.Equal(t, "", *hook.DialogueText)

	// The row carries no tool, thinking, token, or billing identity.
	assert.Empty(t, hook.ToolCalls)
	assert.Empty(t, hook.ToolResults)
	assert.Empty(t, hook.ThinkingText)
	assert.False(t, hook.HasThinking)
	assert.False(t, hook.HasToolUse)
	assert.Empty(t, hook.Model)
	assert.Empty(t, hook.ProviderID)
	assert.Empty(t, hook.TokenUsage)
	assert.Empty(t, hook.ClaudeMessageID)

	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "hello", sess.FirstMessage)
}

// The stdout and stderr channels keep their own sections, and a
// metadata value keeps the whitespace it was written with.
func TestClaudeHookImportRendersStdoutAndStderrChannels(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-01-01T00:00:00Z"),
		`{"type":"attachment","timestamp":"2026-01-01T00:00:01Z",`+
			`"attachment":{"type":"hook_success","hookName":"  spaced  ",`+
			`"stdout":"{\"status\":\"ok\"}","stderr":"boom"}}`,
		testjsonl.ClaudeAssistantJSON("hi", "2026-01-01T00:00:02Z"),
	)

	_, msgs := runClaudeParserTest(t, "hook-stdout.jsonl", content)
	require.Len(t, msgs, 3)

	hook := msgs[1]
	assert.Equal(t,
		"Hook name:   spaced  \n\nStdout:\n{\"status\":\"ok\"}\n\n"+
			"Stderr:\nboom",
		hook.Content)
	assert.Equal(t, len(hook.Content), hook.ContentLength)
}

// Multi-byte text and combining marks survive the body unchanged.
func TestClaudeHookImportKeepsUnicodeHookContent(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-01-01T00:00:00Z"),
		`{"type":"attachment","timestamp":"2026-01-01T00:00:01Z",`+
			`"attachment":{"type":"hook_success",`+
			`"content":"  第一行\n第二行 e\u0301\n"}}`,
		testjsonl.ClaudeAssistantJSON("hi", "2026-01-01T00:00:02Z"),
	)

	_, msgs := runClaudeParserTest(t, "hook-unicode.jsonl", content)
	require.Len(t, msgs, 3)

	hook := msgs[1]
	assert.Equal(t,
		"Content:\n  第一行\n第二行 e\u0301\n", hook.Content)
	assert.Equal(t, len(hook.Content), hook.ContentLength)
}

// A hook written before the first real user stays a system row: it is
// neither the session preview nor a user turn.
func TestClaudeHookImportBeforeFirstUserKeepsPreviewAndCounts(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		`{"type":"attachment","timestamp":"2026-01-01T00:00:00Z",`+
			`"attachment":{"type":"hook_success","content":"hook ran"}}`,
		testjsonl.ClaudeUserJSON("real question", "2026-01-01T00:00:01Z"),
		testjsonl.ClaudeAssistantJSON("hi", "2026-01-01T00:00:02Z"),
	)

	sess, msgs := runClaudeParserTest(t, "hook-before-user.jsonl", content)
	require.Len(t, msgs, 3)

	assert.True(t, msgs[0].IsSystem)
	assert.Equal(t, "hook_success", msgs[0].SourceSubtype)
	assert.Equal(t, "real question", sess.FirstMessage)
	assert.Equal(t, 1, sess.UserMessageCount)
}

// A hook between two replies changes neither the user turn count nor
// the assistant usage of the session.
func TestClaudeHookImportBetweenRepliesKeepsCountsAndUsage(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("first", "2026-01-01T00:00:00Z"),
		`{"type":"assistant","timestamp":"2026-01-01T00:00:01Z",`+
			`"message":{"id":"msg_1","content":[{"type":"text","text":"first reply"}],`+
			`"usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"attachment","timestamp":"2026-01-01T00:00:02Z",`+
			`"attachment":{"type":"hook_success","content":"hook ran"}}`,
		testjsonl.ClaudeUserJSON("second", "2026-01-01T00:00:03Z"),
		`{"type":"assistant","timestamp":"2026-01-01T00:00:04Z",`+
			`"message":{"id":"msg_2","content":[{"type":"text","text":"second reply"}],`+
			`"usage":{"input_tokens":20,"output_tokens":7}}}`,
	)

	sess, msgs := runClaudeParserTest(t, "hook-between.jsonl", content)
	require.Len(t, msgs, 5)

	assert.Equal(t, 2, sess.UserMessageCount)
	assert.Equal(t, "first", sess.FirstMessage)
	assert.Equal(t, 12, sess.TotalOutputTokens)
	assert.Equal(t, 20, sess.PeakContextTokens)
	assert.Equal(t, "hook_success", msgs[2].SourceSubtype)
	assert.Equal(t, RoleUser, msgs[3].Role)
	assert.Equal(t, "second", msgs[3].Content)
}

// A hook and a queued prompt share one timestamp-ordered stream: the
// regular message stays ahead of them and the attachments keep their
// source order.
func TestClaudeHookImportMixedWithQueuedCommandKeepsSourceOrder(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-01-01T00:00:01Z"),
		testjsonl.ClaudeQueuedCommandJSON(
			"queued prompt", "2026-01-01T00:00:01Z",
		),
		`{"type":"attachment","timestamp":"2026-01-01T00:00:01Z",`+
			`"attachment":{"type":"hook_success","content":"hook ran"}}`,
		testjsonl.ClaudeAssistantJSON("ok", "2026-01-01T00:00:02Z"),
	)

	sess, msgs := runClaudeParserTest(t, "hook-mixed.jsonl", content)
	require.Len(t, msgs, 4)

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.False(t, msgs[0].IsSystem)
	assert.Equal(t, "queued_command", msgs[1].SourceSubtype)
	assert.Equal(t, "hook_success", msgs[2].SourceSubtype)
	assert.Equal(t, RoleAssistant, msgs[3].Role)
	for i, m := range msgs {
		assert.Equal(t, i, m.Ordinal, "ordinal mismatch at %d", i)
	}
	assert.Equal(t, 2, sess.UserMessageCount)
	assert.Equal(t, "hello", sess.FirstMessage)
}

// A hook without a timestamp keeps the conservative queued-command
// policy: it cannot be placed against the messages, so it sorts after
// them.
func TestClaudeHookImportWithoutTimestampSortsAfterMessages(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-01-01T00:00:00Z"),
		`{"type":"attachment","attachment":{"type":"hook_success",`+
			`"content":"hook ran"}}`,
		testjsonl.ClaudeAssistantJSON("ok", "2026-01-01T00:00:01Z"),
	)

	_, msgs := runClaudeParserTest(t, "hook-no-timestamp.jsonl", content)
	require.Len(t, msgs, 3)

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "hook_success", msgs[2].SourceSubtype)
}

// An appended hook produces a message on the incremental path: the
// ordinal continues from the scan start, the consumed offset covers
// the appended line, and the session end covers the hook.
func TestClaudeHookImportIncrementalHookOnlyAppend(t *testing.T) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-01-01T00:00:00Z"),
		testjsonl.ClaudeAssistantJSON("hi", "2026-01-01T00:00:01Z"),
	)
	path := createTestFile(t, "hook-incremental.jsonl", initial)
	info, err := os.Stat(path)
	require.NoError(t, err)

	appended := `{"type":"attachment","timestamp":"2026-01-01T00:00:02Z",` +
		`"attachment":{"type":"hook_success","content":"hook ran"}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	msgs, endedAt, consumed, err := callParseClaudeSessionFrom(
		path, info.Size(), 2, "",
	)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	hook := msgs[0]
	assert.Equal(t, 2, hook.Ordinal)
	assert.Equal(t, "hook_success", hook.SourceSubtype)
	assert.Equal(t, "attachment", hook.SourceType)
	assert.True(t, hook.IsSystem)
	assert.Equal(t, RoleUser, hook.Role)
	assert.Equal(t, int64(len(appended)), consumed)
	assert.Equal(t,
		"2026-01-01T00:00:02Z", endedAt.Format(time.RFC3339))
}

// A hook written between the chunks of one response must not hide the
// response head from the cross-sync split detection, so the append
// falls back to a full parse when the head continues the stored tail.
// A fresh response id stays incremental.
func TestClaudeHookImportSplitDetectionFallback(t *testing.T) {
	t.Parallel()

	chunk := func(uuid, parent, ts, text string) string {
		return `{"type":"assistant","uuid":"` + uuid +
			`","parentUuid":"` + parent +
			`","timestamp":"` + ts +
			`","message":{"id":"msg_split","content":[` +
			`{"type":"text","text":"` + text + `"}]}}`
	}

	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-01-01T10:00:00Z"),
		chunk("a1", "u1", "2026-01-01T10:00:01Z", "Hel"),
	)

	parseFrom := func(
		t *testing.T, appended string, tailID *string,
	) ([]ParsedMessage, error) {
		t.Helper()

		path := createTestFile(t, "hook-boundary.jsonl", initial)
		info, err := os.Stat(path)
		require.NoError(t, err)

		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		_, err = f.WriteString(appended)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		linear := true
		msgs, _, _, _, perr := claudeParseSessionFrom(
			path, info.Size(), claudeIncrementalScan{
				startOrdinal:              2,
				lastEntryUUID:             "a1",
				storedLinearParse:         &linear,
				storedTailClaudeMessageID: tailID,
			},
		)
		return msgs, perr
	}

	hookLine := `{"type":"attachment","timestamp":"2026-01-01T10:00:02Z",` +
		`"attachment":{"type":"hook_success","content":"hook ran"}}` + "\n"

	t.Run("hook ahead of the continuation head forces a full parse",
		func(t *testing.T) {
			t.Parallel()

			appended := hookLine +
				chunk("a2", "a1", "2026-01-01T10:00:03Z", "Hello wo") +
				"\n" +
				chunk("a3", "a2", "2026-01-01T10:00:04Z", "Hello world") +
				"\n"

			tail := "msg_split"
			_, err := parseFrom(t, appended, &tail)
			assert.ErrorIs(t, err, ErrClaudeIncrementalNeedsFullParse,
				"a hook sorting ahead of a same-message.id continuation "+
					"must force a full parse")
		})

	t.Run("hook ahead of a fresh-id response stays incremental",
		func(t *testing.T) {
			t.Parallel()

			appended := hookLine +
				`{"type":"assistant","uuid":"a2","parentUuid":"a1",` +
				`"timestamp":"2026-01-01T10:00:03Z",` +
				`"message":{"id":"msg_fresh","content":[` +
				`{"type":"text","text":"fresh response"}]}}` + "\n"

			tail := "msg_split"
			msgs, err := parseFrom(t, appended, &tail)
			require.NoError(t, err,
				"a fresh-id head cannot be a hidden continuation of the "+
					"stored tail, so the append must stay incremental")
			require.Len(t, msgs, 2)
			assert.Equal(t, "hook_success", msgs[0].SourceSubtype)
			assert.Equal(t, "msg_fresh", msgs[1].ClaudeMessageID)
		})
}

// An unknown native parent cannot be assigned to a branch from the
// appended window alone, so the incremental path falls back to a full
// parse.
func TestClaudeHookImportIncrementalParentedHookFallsBack(t *testing.T) {
	t.Parallel()

	initial := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-01-01T10:00:00Z"),
	)
	path := createTestFile(t, "hook-parented.jsonl", initial)
	info, err := os.Stat(path)
	require.NoError(t, err)

	appended := `{"type":"attachment","timestamp":"2026-01-01T10:00:01Z",` +
		`"uuid":"hook-1","parentUuid":"fork-u1",` +
		`"attachment":{"type":"hook_success","content":"hook ran"}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	linear := true
	_, _, _, _, err = claudeParseSessionFrom(
		path, info.Size(), claudeIncrementalScan{
			startOrdinal:      1,
			lastEntryUUID:     "u1",
			storedLinearParse: &linear,
		},
	)
	assert.ErrorIs(t, err, ErrClaudeIncrementalNeedsFullParse)
}

// A known parent on a stored linear source does not require reparsing
// the entire transcript, including when one hook parents another.
func TestClaudeHookImportKnownLinearParentsStayIncremental(t *testing.T) {
	t.Parallel()

	const initial = `{"type":"user","uuid":"known-user","parentUuid":"missing-ancestor","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"request"}}` + "\n"
	const hook = `{"type":"attachment","uuid":"hook-1","parentUuid":"known-user","timestamp":"2026-01-01T00:00:01Z","attachment":{"type":"hook_success","stdout":"first output"}}` + "\n"
	for _, tc := range []struct {
		name     string
		appended string
		rows     int
	}{
		{name: "stored tail", appended: hook, rows: 1},
		{
			name: "appended message",
			appended: `{"type":"assistant","uuid":"new-answer","parentUuid":"known-user","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":"answer"}}` + "\n" +
				`{"type":"attachment","uuid":"hook-1","parentUuid":"new-answer","timestamp":"2026-01-01T00:00:02Z","attachment":{"type":"hook_success","stdout":"output"}}` + "\n",
			rows: 2,
		},
		{
			name: "earlier hook",
			appended: hook +
				`{"type":"attachment","uuid":"hook-2","parentUuid":"hook-1","timestamp":"2026-01-01T00:00:02Z","attachment":{"type":"hook_success","stdout":"second output"}}` + "\n",
			rows: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := createTestFile(t, "hook-known-parent.jsonl", initial)
			info, err := os.Stat(path)
			require.NoError(t, err)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			require.NoError(t, err)
			_, err = f.WriteString(tc.appended)
			require.NoError(t, err)
			require.NoError(t, f.Close())

			linear := true
			messages, _, endedAt, consumed, err := claudeParseSessionFrom(
				path, info.Size(), claudeIncrementalScan{
					startOrdinal: 1, lastEntryUUID: "known-user",
					storedLinearParse: &linear,
				},
			)
			require.NoError(t, err)
			require.Len(t, messages, tc.rows)
			assert.Equal(t, int64(len(tc.appended)), consumed)
			assert.Equal(t, "hook_success", messages[tc.rows-1].SourceSubtype)
			assert.Equal(t, tc.rows, messages[tc.rows-1].Ordinal)
			assert.Equal(t, messages[tc.rows-1].Timestamp, endedAt)
		})
	}
}

func TestClaudeHookImportKeepsPromptLikeContentLiteral(t *testing.T) {
	t.Parallel()
	content := testjsonl.JoinJSONL(
		`{"type":"attachment","timestamp":"2026-01-01T00:00:00Z","attachment":{"type":"hook_success","content":"<system-reminder>\n  Keep this literal.\n</system-reminder>"}}`,
		testjsonl.ClaudeUserJSON("actual prompt", "2026-01-01T00:00:01Z"),
	)
	sess, messages := runClaudeParserTest(t, "hook-literal.jsonl", content)
	require.Len(t, messages, 2)
	assert.Equal(t, "Content:\n<system-reminder>\n  Keep this literal.\n</system-reminder>", messages[0].Content)
	assert.True(t, messages[0].IsSystem)
	assert.Equal(t, "actual prompt", sess.FirstMessage)
	assert.Equal(t, 1, sess.UserMessageCount)
}

// The full parse keeps one merged assistant message when a hook sits
// between two snapshots of the same response.
func TestClaudeHookImportFullParseMergesSnapshotsAcrossHook(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-01-01T00:00:00Z"),
		`{"type":"assistant","uuid":"a1","parentUuid":"u1",`+
			`"timestamp":"2026-01-01T00:00:01Z",`+
			`"message":{"id":"msg_run","content":[`+
			`{"type":"text","text":"Hi"}]}}`,
		`{"type":"attachment","timestamp":"2026-01-01T00:00:02Z",`+
			`"attachment":{"type":"hook_success","content":"hook ran"}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"a1",`+
			`"timestamp":"2026-01-01T00:00:03Z",`+
			`"message":{"id":"msg_run","content":[`+
			`{"type":"text","text":"Hi there"}]}}`,
	)

	_, msgs := runClaudeParserTest(t, "hook-merge.jsonl", content)
	require.Len(t, msgs, 3)

	assert.Equal(t, "hook_success", msgs[1].SourceSubtype)
	assert.Equal(t, "Hi there", msgs[2].Content)
	assert.Equal(t, "msg_run", msgs[2].ClaudeMessageID)
	ids := 0
	for _, m := range msgs {
		if m.ClaudeMessageID == "msg_run" {
			ids++
		}
	}
	assert.Equal(t, 1, ids,
		"the response must stay one message after the hook")
}

// An unsupported attachment type and a hook without output stay
// skipped.
func TestClaudeHookImportSkipsUnsupportedAndBlankHooks(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		testjsonl.ClaudeUserJSON("hello", "2026-01-01T00:00:00Z"),
		`{"type":"attachment","timestamp":"2026-01-01T00:00:01Z",`+
			`"attachment":{"type":"hook_error","content":"failed"}}`,
		`{"type":"attachment","timestamp":"2026-01-01T00:00:02Z",`+
			`"attachment":{"type":"hook_success","content":"   "}}`,
		`{"type":"attachment","timestamp":"2026-01-01T00:00:03Z",`+
			`"attachment":{"type":"hook_success"}}`,
		testjsonl.ClaudeAssistantJSON("ok", "2026-01-01T00:00:04Z"),
	)

	sess, msgs := runClaudeParserTest(t, "hook-unsupported.jsonl", content)
	require.Len(t, msgs, 2)

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Empty(t, claudeHookImportRows(msgs))
	assert.Equal(t, 1, sess.UserMessageCount)
}

// A hook without a native parent belongs to the original result only,
// and a hook whose native parent sits on a fork belongs to that fork.
func TestClaudeHookImportForkOwnership(t *testing.T) {
	t.Parallel()

	content := testjsonl.JoinJSONL(
		`{"type":"user","timestamp":"2026-01-01T10:00:00Z","uuid":"a",`+
			`"message":{"content":"start"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:01Z",`+
			`"uuid":"b","parentUuid":"a","message":{"content":[`+
			`{"type":"text","text":"ok"}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:02Z","uuid":"c",`+
			`"parentUuid":"b","message":{"content":"q1"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:03Z",`+
			`"uuid":"d","parentUuid":"c","message":{"content":[`+
			`{"type":"text","text":"a1"}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:04Z","uuid":"e",`+
			`"parentUuid":"d","message":{"content":"q2"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:05Z",`+
			`"uuid":"f","parentUuid":"e","message":{"content":[`+
			`{"type":"text","text":"a2"}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:06Z","uuid":"g",`+
			`"parentUuid":"f","message":{"content":"q3"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:07Z",`+
			`"uuid":"h","parentUuid":"g","message":{"content":[`+
			`{"type":"text","text":"a3"}]}}`,
		`{"type":"user","timestamp":"2026-01-01T10:00:08Z","uuid":"k",`+
			`"parentUuid":"h","message":{"content":"q4"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:00:09Z",`+
			`"uuid":"l","parentUuid":"k","message":{"content":[`+
			`{"type":"text","text":"a4"}]}}`,
		// The fork branch hangs off the assistant at 10:00:01.
		`{"type":"user","timestamp":"2026-01-01T10:01:00Z",`+
			`"uuid":"fork-u1","parentUuid":"b",`+
			`"message":{"content":"forked question"}}`,
		`{"type":"assistant","timestamp":"2026-01-01T10:01:01Z",`+
			`"uuid":"fork-a1","parentUuid":"fork-u1",`+
			`"message":{"content":[{"type":"text","text":"forked answer"}]}}`,
		// A hook without a native parent stays with the original
		// result.
		`{"type":"attachment","timestamp":"2026-01-01T10:00:10Z",`+
			`"attachment":{"type":"hook_success",`+
			`"content":"unparented hook"}}`,
		// A hook whose native parent sits on the fork belongs to the
		// fork result.
		`{"type":"attachment","timestamp":"2026-01-01T10:01:02Z",`+
			`"uuid":"hook-1","parentUuid":"fork-u1",`+
			`"attachment":{"type":"hook_success","content":"fork hook"}}`,
		`{"type":"attachment","timestamp":"2026-01-01T10:01:03Z",`+
			`"uuid":"hook-2","parentUuid":"hook-1",`+
			`"attachment":{"type":"hook_success","content":"chained fork hook"}}`,
	)

	path := createTestFile(t, "hook-fork.jsonl", content)
	results, err := parseClaudeSession(path, "proj", "local")
	require.NoError(t, err)
	require.Len(t, results, 2)

	originalHooks := claudeHookImportRows(results[0].Messages)
	forkHooks := claudeHookImportRows(results[1].Messages)
	require.Len(t, originalHooks, 1,
		"the unparented hook belongs to the original result only")
	require.Len(t, forkHooks, 2,
		"the parented hook and its child belong to the fork result")
	assert.Contains(t, originalHooks[0].Content, "unparented hook")
	assert.NotContains(t, originalHooks[0].Content, "fork hook")
	assert.Contains(t, forkHooks[0].Content, "fork hook")
	assert.NotContains(t, forkHooks[0].Content, "unparented hook")
	assert.Equal(t, "fork-u1", forkHooks[0].SourceParentUUID)
	assert.Equal(t, "hook-1", forkHooks[1].SourceParentUUID)
	assert.Equal(t, "hook-2", forkHooks[1].SourceUUID)
	assert.Contains(t, forkHooks[1].Content, "chained fork hook")

	// The hooks are system rows: they add no user turn.
	assert.Equal(t, 5, results[0].Session.UserMessageCount)
	assert.Equal(t, 1, results[1].Session.UserMessageCount)
}
