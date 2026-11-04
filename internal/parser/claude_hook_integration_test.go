package parser

import (
	"fmt"
	"os"
	"strings"
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
		if m.SourceSubtype == "hook_success" || m.SourceSubtype == "hook_additional_context" {
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
	for _, tc := range []struct{ subtype, content, body string }{
		{"hook_success", `"hook finished"`, "hook finished"},
		{"hook_additional_context", `["hook finished","more context"]`, "hook finished\nmore context"},
	} {
		t.Run(tc.subtype, func(t *testing.T) {

			content := testjsonl.JoinJSONL(
				testjsonl.ClaudeUserJSON("hello", "2026-01-01T00:00:00Z"),
				`{"type":"attachment","timestamp":"2026-01-01T00:00:01Z",`+
					`"uuid":"hook-row","parentUuid":"native-parent",`+
					`"isSidechain":true,"promptSource":"synthetic-origin",`+
					`"attachment":{"type":"`+tc.subtype+`","hookName":"example-hook",`+
					`"hookEvent":"PostToolUse","content":`+tc.content+`}}`,
				testjsonl.ClaudeAssistantJSON("hi", "2026-01-01T00:00:02Z"),
			)

			sess, msgs := runClaudeParserTest(t, "hook-body.jsonl", content)
			require.Len(t, msgs, 3)

			hook := msgs[1]
			assert.Equal(t, RoleUser, hook.Role)
			assert.True(t, hook.IsSystem)
			assert.Equal(t, "attachment", hook.SourceType)
			assert.Equal(t, tc.subtype, hook.SourceSubtype)
			assert.Equal(t, "hook-row", hook.SourceUUID)
			assert.Equal(t, "native-parent", hook.SourceParentUUID)
			assert.True(t, hook.IsSidechain)
			assert.Equal(t, "synthetic-origin", hook.PromptSource)
			assert.Equal(t,
				"Hook name: example-hook\n\nHook event: PostToolUse\n\n"+
					"Content:\n"+tc.body,
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
		})
	}
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
	for _, tc := range []struct{ subtype, content, body string }{
		{"hook_success", `"hook finished"`, "hook finished"},
		{"hook_additional_context", `["hook finished","more context"]`, "hook finished\nmore context"},
	} {
		t.Run(tc.subtype, func(t *testing.T) {

			initial := testjsonl.JoinJSONL(
				testjsonl.ClaudeUserJSON("hello", "2026-01-01T00:00:00Z"),
				testjsonl.ClaudeAssistantJSON("hi", "2026-01-01T00:00:01Z"),
			)
			path := createTestFile(t, "hook-incremental.jsonl", initial)
			info, err := os.Stat(path)
			require.NoError(t, err)

			appended := `{"type":"attachment","timestamp":"2026-01-01T00:00:02Z",` +
				`"uuid":"hook-1","parentUuid":"unknown-parent",` +
				`"attachment":{"type":"` + tc.subtype + `","content":` + tc.content + `}}` + "\n"
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
			assert.Equal(t, "Content:\n"+tc.body, hook.Content)
			assert.Equal(t, 2, hook.Ordinal)
			assert.Equal(t, tc.subtype, hook.SourceSubtype)
			assert.Equal(t, "attachment", hook.SourceType)
			assert.True(t, hook.IsSystem)
			assert.Equal(t, RoleUser, hook.Role)
			assert.Equal(t, int64(len(appended)), consumed)
			assert.Equal(t,
				"2026-01-01T00:00:02Z", endedAt.Format(time.RFC3339))
		})
	}
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
			t.Context(), path, info.Size(), claudeIncrementalScan{
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
		`{"type":"attachment","attachment":{"type":"hook_additional_context","content":[]}}`,
		`{"type":"attachment","attachment":{"type":"hook_additional_context","content":[""]}}`,
		testjsonl.ClaudeAssistantJSON("ok", "2026-01-01T00:00:04Z"),
	)

	sess, msgs := runClaudeParserTest(t, "hook-unsupported.jsonl", content)
	require.Len(t, msgs, 2)

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Empty(t, claudeHookImportRows(msgs))
	assert.Equal(t, 1, sess.UserMessageCount)
}

// Hooks follow fork parents even when the parent is hidden or another hook.
func TestClaudeHookImportForkOwnership(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, parent, meta string }{
		{name: "fork", parent: "fork"},
		{name: "hidden fork entry", parent: "hidden", meta: `{"type":"user","uuid":"hidden","parentUuid":"fork","isMeta":true,"message":{"content":"hidden context"}}`},
		{name: "attachment chain", parent: "bridge", meta: `{"type":"attachment","uuid":"bridge","parentUuid":"fork","attachment":{"type":"hook_success","content":"bridge"}}`},
		{name: "empty hook bridge", parent: "bridge", meta: `{"type":"attachment","uuid":"bridge","parentUuid":"fork","attachment":{"type":"hook_success","content":""}}`},
		{name: "cancelled hook bridge", parent: "bridge", meta: `{"type":"attachment","uuid":"bridge","parentUuid":"fork","attachment":{"type":"hook_cancelled"}}`},
		{name: "other attachment bridge", parent: "bridge", meta: `{"type":"attachment","uuid":"bridge","parentUuid":"fork","attachment":{"type":"task_reminder"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var content strings.Builder
			content.WriteString(`{"type":"user","uuid":"root","message":{"content":"start"}}` + "\n")
			parent := "root"
			for i := range 5 {
				uuid := fmt.Sprintf("main-%d", i)
				fmt.Fprintf(&content, `{"type":"user","uuid":%q,"parentUuid":%q,"message":{"content":"main prompt"}}`+"\n", uuid, parent)
				parent = uuid
			}
			content.WriteString(`{"type":"user","uuid":"fork","parentUuid":"root","message":{"content":"fork prompt"}}` + "\n")
			if tc.meta != "" {
				content.WriteString(tc.meta + "\n")
			}
			fmt.Fprintf(&content, `{"type":"attachment","uuid":"hook","parentUuid":%q,"attachment":{"type":"hook_additional_context","content":["fork context"]}}`+"\n", tc.parent)
			path := createTestFile(t, "hook-fork.jsonl", content.String())
			results, err := parseClaudeSession(path, "project", "local")
			require.NoError(t, err)
			require.Len(t, results, 2)
			assert.Empty(t, claudeHookImportRows(results[0].Messages))
			var hooks []ParsedMessage
			for _, m := range results[1].Messages {
				if m.SourceUUID == "hook" {
					hooks = append(hooks, m)
				}
				assert.NotEqual(t, "hidden", m.SourceUUID)
			}
			require.Len(t, hooks, 1)
			assert.Equal(t, "Content:\nfork context", hooks[0].Content)
		})
	}
}
