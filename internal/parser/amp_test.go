package parser

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func runAmpParserTest(
	t *testing.T, content string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()
	path := createTestFile(t, "T-test.json", content)
	return parseAmpTestSession(t, path, "local")
}

func parseAmpTestSession(
	t *testing.T,
	path string,
	machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()

	provider, ok := NewProvider(AgentAmp, ProviderConfig{
		Roots:   []string{filepath.Dir(path)},
		Machine: machine,
	})
	require.True(t, ok)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: SourceRef{
			Provider:       AgentAmp,
			Key:            path,
			DisplayPath:    path,
			FingerprintKey: path,
			Opaque: JSONLSource{
				Root: filepath.Dir(path),
				Path: path,
			},
		},
		Machine: machine,
	})
	if err != nil || len(outcome.Results) == 0 {
		return nil, nil, err
	}
	result := outcome.Results[0].Result
	return &result.Session, result.Messages, nil
}

func TestAmpProviderParsesBasic(t *testing.T) {
	threadID := "T-019ca26f-aaaa-bbbb-cccc-dddddddddddd"
	content := `{
		"v": 1,
		"id": "` + threadID + `",
		"created": 1704067200000,
		"title": "Migrate database schema",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Migrate the DB schema."}]},
			{"role": "assistant", "content": [{"type": "text", "text": "Sure, I will help."}]}
		],
		"env": {
			"initial": {
				"trees": [{"displayName": "myproject", "uri": "file:///home/user/myproject"}]
			}
		},
		"meta": {
			"traces": [
				{"name": "inference", "startTime": "2024-01-01T00:00:01Z", "endTime": "2024-01-01T00:00:02Z"},
				{"name": "tools",     "startTime": "2024-01-01T00:00:03Z", "endTime": "2024-01-01T00:00:05Z"}
			]
		}
	}`

	path := createTestFile(t, threadID+".json", content)
	sess, msgs, err := parseAmpTestSession(t, path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"amp:"+threadID,
		"myproject", AgentAmp,
	)

	// Title takes precedence as FirstMessage.
	assert.Equal(t, "Migrate database schema", sess.FirstMessage)
	assertMessageCount(t, sess.MessageCount, 2)
	assert.Equal(t, 1, sess.UserMessageCount)

	// Start time from created (epoch ms).
	wantStart := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	assertTimestamp(t, sess.StartedAt, wantStart)

	// End time from meta.traces[last].endTime.
	wantEnd := time.Date(2024, 1, 1, 0, 0, 5, 0, time.UTC)
	assertTimestamp(t, sess.EndedAt, wantEnd)

	require.Equal(t, 2, len(msgs))
	assertMessage(t, msgs[0], RoleUser, "Migrate the DB schema.")
	assertMessage(t, msgs[1], RoleAssistant, "Sure, I will help.")
	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.Equal(t, 1, msgs[1].Ordinal)
}

func TestAmpProviderParsesToolUseAndThinking(t *testing.T) {
	content := `{
		"v": 1,
		"id": "T-tooluse",
		"created": 1704067200000,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Read the file."}]},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "Let me plan this."},
				{"type": "text", "text": "I will read it now."},
				{"type": "tool_use", "complete": true, "id": "tu1", "name": "Read", "input": {"file_path": "main.go"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu1", "content": "package main"}
			]}
		],
		"env": {"initial": {"trees": [{"displayName": "myrepo"}]}},
		"meta": {"traces": []}
	}`

	sess, msgs, err := runAmpParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertMessageCount(t, sess.MessageCount, 3)
	// tool_result-only user messages have no text content,
	// so only the first user message (with text) is counted.
	assert.Equal(t, 1, sess.UserMessageCount)

	assert.False(t, msgs[0].HasThinking)
	assert.False(t, msgs[0].HasToolUse)

	assert.True(t, msgs[1].HasThinking)
	assert.True(t, msgs[1].HasToolUse)
	assert.Contains(t, msgs[1].Content, "[Thinking]")
	assert.Contains(t, msgs[1].Content, "Let me plan this.")
	assert.Contains(t, msgs[1].Content, "[Read: main.go]")

	require.Equal(t, 1, len(msgs[1].ToolCalls))
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].Category)
	assert.Equal(t, "tu1", msgs[1].ToolCalls[0].ToolUseID)

	// tool_result message: content should be empty (no text blocks),
	// but tool results are recorded.
	assert.Equal(t, 1, len(msgs[2].ToolResults))
	assert.Equal(t, "tu1", msgs[2].ToolResults[0].ToolUseID)

	// EndTime absent (empty traces) → zero.
	assertZeroTimestamp(t, sess.EndedAt, "EndedAt")
}

func TestSerializeAmpResult(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain string", input: `"some text"`, want: "some text"},
		{name: "empty string", input: `""`, want: ""},
		{name: "number", input: `123`, want: "123"},
		{name: "bool true", input: `true`, want: "true"},
		{name: "bool false", input: `false`, want: "false"},
		{name: "bash dict", input: `{"output":"hello\n","exitCode":0}`, want: "hello\n"},
		{name: "bash dict output number", input: `{"output":0,"exitCode":0}`, want: "0"},
		{name: "read dict", input: `{"absolutePath":"/a/b","content":"file data"}`, want: "file data"},
		{name: "read dict content bool", input: `{"absolutePath":"/a/b","content":false}`, want: "false"},
		{name: "edit dict", input: `{"diff":"--- a\n+++ b"}`, want: "--- a\n+++ b"},
		{name: "success true", input: `{"success":true}`, want: "success"},
		{name: "success false", input: `{"success":false}`, want: "failed"},
		{name: "unknown dict fallback", input: `{"foo":"bar"}`, want: `{"foo":"bar"}`},
		{name: "grep list strings", input: `["/a:1:x","/b:2:y"]`, want: "/a:1:x\n/b:2:y"},
		{name: "mixed list string number", input: `["a",1]`, want: `["a",1]`},
		{name: "mixed list string bool string", input: `["a",false,"b"]`, want: `["a",false,"b"]`},
		{name: "screenshot list objects", input: `[{"type":"image","data":"..."}]`, want: "[binary content]"},
		{name: "list of non-image objects", input: `[{"file":"foo.go","line":42},{"file":"bar.go","line":7}]`, want: `[{"file":"foo.go","line":42},{"file":"bar.go","line":7}]`},
		{name: "list numbers", input: `[1,2,3]`, want: `[1,2,3]`},
		{name: "empty array", input: `[]`, want: ""},
		{name: "null", input: `null`, want: ""},
		{name: "null output falls through to content", input: `{"output":null,"content":"fallback"}`, want: "fallback"},
		{name: "null output falls through to diff", input: `{"output":null,"diff":"--- a"}`, want: "--- a"},
		{name: "empty output falls through to content", input: `{"output":"","content":"fallback"}`, want: "fallback"},
		{name: "empty output no fallback returns empty", input: `{"output":"","exitCode":0}`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := gjson.Parse(`{"result":` + tt.input + `}`).Get("result")
			assert.Equal(t, tt.want, serializeAmpResult(result))
		})
	}

	t.Run("not exists", func(t *testing.T) {
		assert.Equal(t, "", serializeAmpResult(gjson.Result{}))
	})
}

func TestExtractAmpToolResults(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantN  int
		wantID string
		want   string
	}{
		{
			name:  "canonical tool_use_id skipped",
			input: `[{"type":"tool_result","tool_use_id":"tu1","content":"x"}]`,
			wantN: 0,
		},
		{
			name:   "amp string result",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":"text"}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "text",
		},
		{
			name:   "amp number result",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":0}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "0",
		},
		{
			name:   "amp bool result",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":false}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "false",
		},
		{
			name:   "amp dict result",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":{"output":"ok","exitCode":0}}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "ok",
		},
		{
			name:   "amp dict output number",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":{"output":0,"exitCode":0}}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "0",
		},
		{
			name:   "amp list number result",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":[1,2,3]}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "[1,2,3]",
		},
		{
			name:   "amp error result",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"error","error":{"message":"fail"}}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "fail",
		},
		{
			name:   "amp unknown error",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"error"}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "[unknown error]",
		},
		{
			name:   "amp cancelled",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"cancelled"}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "[cancelled]",
		},
		{
			name:  "amp null result done",
			input: `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":null}}]`,
			wantN: 0,
		},
		{
			name:  "amp missing result and status",
			input: `[{"type":"tool_result","toolUseID":"tu1","run":{"foo":"bar"}}]`,
			wantN: 0,
		},
		{
			// Empty array result: run.result existed so the block is preserved
			// even though serialized text is empty, avoiding a "pending" tool call.
			name:   "amp empty array result",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":[]}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "",
		},
		{
			// Empty string result: same as above — result existed, preserve it.
			name:   "amp empty string result",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":""}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "",
		},
		{
			// Array of non-image objects should round-trip as raw JSON, not "[binary content]".
			name:   "amp list of objects result",
			input:  `[{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":[{"file":"foo.go","line":42}]}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   `[{"file":"foo.go","line":42}]`,
		},
		{
			name:   "empty tool_use_id falls through to toolUseID",
			input:  `[{"type":"tool_result","tool_use_id":"","toolUseID":"tu1","run":{"status":"done","result":"text"}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "text",
		},
		{
			name:   "null tool_use_id falls through to toolUseID",
			input:  `[{"type":"tool_result","tool_use_id":null,"toolUseID":"tu1","run":{"status":"done","result":"ok"}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "ok",
		},
		{
			name:  "non tool_result block",
			input: `[{"type":"text","text":"hello"}]`,
			wantN: 0,
		},
		{
			name:   "mixed canonical and amp",
			input:  `[{"type":"tool_result","tool_use_id":"tu0","content":"x"},{"type":"tool_result","toolUseID":"tu1","run":{"status":"done","result":"text"}}]`,
			wantN:  1,
			wantID: "tu1",
			want:   "text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractAmpToolResults(gjson.Parse(tt.input))
			assert.Equal(t, tt.wantN, len(got))
			if tt.wantN == 0 {
				return
			}

			assert.Equal(t, tt.wantID, got[0].ToolUseID)
			assert.Equal(t, tt.want, DecodeContent(got[0].ContentRaw))
			assert.Equal(t, len(tt.want), got[0].ContentLength)
		})
	}
}

func TestAmpProviderParsesAmpToolResultSchema(t *testing.T) {
	content := `{
		"v": 1,
		"id": "T-amp-tool-result-schema",
		"created": 1704067200000,
		"messages": [
			{"role":"assistant","content":[
				{"type":"text","text":"running librarian"},
				{"type":"tool_use","id":"toolu1","name":"librarian","input":{"query":"x"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","toolUseID":"toolu1","run":{"status":"done","result":"Here is a complete breakdown"}}
			]}
		]
	}`

	sess, msgs, err := runAmpParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolResults, 1)
	assert.Equal(t, "toolu1", msgs[1].ToolResults[0].ToolUseID)
	assert.Equal(t, "Here is a complete breakdown", DecodeContent(msgs[1].ToolResults[0].ContentRaw))
}

func TestAmpProviderParsesAmpToolResultDict(t *testing.T) {
	content := `{
		"v": 1,
		"id": "T-amp-tool-result-dict",
		"created": 1704067200000,
		"messages": [
			{"role":"assistant","content":[
				{"type":"text","text":"running bash"},
				{"type":"tool_use","id":"toolu1","name":"Bash","input":{"command":"echo ok"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","toolUseID":"toolu1","run":{"status":"done","result":{"output":"cmd output","exitCode":0}}}
			]}
		]
	}`

	_, msgs, err := runAmpParserTest(t, content)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[1].ToolResults, 1)
	assert.Equal(t, "cmd output", DecodeContent(msgs[1].ToolResults[0].ContentRaw))
}

func TestAmpProviderParsesNoEnv(t *testing.T) {
	content := `{
		"v": 1,
		"id": "T-noenv",
		"created": 1704067200000,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hello"}]}
		]
	}`

	sess, msgs, err := runAmpParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// No env → project falls back to "amp".
	assert.Equal(t, "amp", sess.Project)
	require.Equal(t, 1, len(msgs))
}

func TestAmpProviderParsesNoTitle(t *testing.T) {
	content := `{
		"v": 1,
		"id": "T-notitle",
		"created": 1704067200000,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Fix the bug in main.go please."}]},
			{"role": "assistant", "content": [{"type": "text", "text": "Done."}]}
		]
	}`

	sess, _, err := runAmpParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// No title → FirstMessage extracted from first user message.
	assert.Equal(t, "Fix the bug in main.go please.", sess.FirstMessage)
}

func TestAmpProviderParsesNoMetaTraces(t *testing.T) {
	content := `{
		"v": 1,
		"id": "T-notraces",
		"created": 1704067200000,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hello"}]}
		],
		"meta": {}
	}`

	sess, _, err := runAmpParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// No traces → EndedAt is zero.
	assertZeroTimestamp(t, sess.EndedAt, "EndedAt")
}

func TestAmpProviderParsesLastTraceWithoutEndTime(t *testing.T) {
	content := `{
		"v": 1,
		"id": "T-trace-end-missing",
		"created": 1704067200000,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hello"}]}
		],
		"meta": {
			"traces": [
				{"name": "inference", "endTime": "2024-01-01T00:00:02Z"},
				{"name": "tools"}
			]
		}
	}`

	sess, _, err := runAmpParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Earlier trace endTime is used when the last trace has none.
	assert.Equal(t, "2024-01-01T00:00:02Z", sess.EndedAt.UTC().Format(time.RFC3339))
}

func TestAmpProviderParsesEmptyThread(t *testing.T) {
	content := `{
		"v": 1,
		"id": "T-empty",
		"created": 1704067200000,
		"messages": []
	}`

	sess, msgs, err := runAmpParserTest(t, content)
	require.NoError(t, err)
	// Empty thread → nil, nil, nil (non-interactive).
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestAmpProviderParsesFirstMessageTruncation(t *testing.T) {
	longText := strings.Repeat("a", 400)
	content := `{"v":1,"id":"T-trunc","created":1704067200000,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"` + longText + `"}]}]}`

	sess, _, err := runAmpParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)
	// truncate clips at 300 chars + 3 ellipsis chars = 303.
	assert.Equal(t, 303, len(sess.FirstMessage))
}

func TestAmpProviderParsesInvalidCreated(t *testing.T) {
	t.Run("missing created", func(t *testing.T) {
		content := `{
			"v": 1,
			"id": "T-missing-created",
			"messages": [
				{"role": "user", "content": [{"type": "text", "text": "hello"}]}
			]
		}`

		sess, _, err := runAmpParserTest(t, content)
		require.NoError(t, err)
		require.NotNil(t, sess)
		assertZeroTimestamp(t, sess.StartedAt, "StartedAt")
	})

	t.Run("non numeric created", func(t *testing.T) {
		content := `{
			"v": 1,
			"id": "T-invalid-created",
			"created": "nope",
			"messages": [
				{"role": "user", "content": [{"type": "text", "text": "hello"}]}
			]
		}`

		sess, _, err := runAmpParserTest(t, content)
		require.NoError(t, err)
		require.NotNil(t, sess)
		assertZeroTimestamp(t, sess.StartedAt, "StartedAt")
	})

	t.Run("non positive numeric created", func(t *testing.T) {
		cases := []struct {
			name    string
			created string
		}{
			{name: "zero", created: "0"},
			{name: "negative", created: "-1"},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				content := `{
					"v": 1,
					"id": "T-invalid-created-numeric",
					"created": ` + tc.created + `,
					"messages": [
						{"role": "user", "content": [{"type": "text", "text": "hello"}]}
					]
				}`

				sess, _, err := runAmpParserTest(t, content)
				require.NoError(t, err)
				require.NotNil(t, sess)
				assertZeroTimestamp(t, sess.StartedAt, "StartedAt")
			})
		}
	})
}

func TestAmpProviderParsesErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, _, err := parseAmpTestSession(t, "/nonexistent/T-xxx.json", "local")
		assert.Error(t, err)
	})

	t.Run("invalid JSON", func(t *testing.T) {
		_, _, err := runAmpParserTest(t, "{not valid json")
		assert.Error(t, err)
	})

	t.Run("missing id falls back to filename", func(t *testing.T) {
		content := `{"v":1,"created":1704067200000,"messages":[` +
			`{"role":"user","content":[{"type":"text","text":"hello"}]}]}`
		sess, _, err := runAmpParserTest(t, content)
		require.NoError(t, err)
		require.NotNil(t, sess)
		// Falls back to filename-derived ID (T-test from T-test.json).
		assert.Equal(t, "amp:T-test", sess.ID)
	})

	t.Run("missing id and invalid filename", func(t *testing.T) {
		content := `{"v":1,"created":1704067200000,"messages":[]}`
		path := createTestFile(t, "bad-name.json", content)
		_, _, err := parseAmpTestSession(t, path, "local")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "missing or invalid id")
	})
}

func TestAmpProviderParsesMismatchedID(t *testing.T) {
	t.Run("invalid JSON id", func(t *testing.T) {
		content := `{
			"v": 1,
			"id": "bogus-not-a-thread-id",
			"created": 1704067200000,
			"messages": [
				{"role": "user", "content": [{"type": "text", "text": "hello"}]}
			]
		}`

		path := createTestFile(t, "T-fallback-uuid.json", content)
		sess, _, err := parseAmpTestSession(t, path, "local")
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, "amp:T-fallback-uuid", sess.ID)
	})

	t.Run("valid JSON id with invalid filename", func(t *testing.T) {
		// Filename doesn't match T-<id>.json, so the parser
		// falls back to the JSON id.
		content := `{
			"v": 1,
			"id": "T-from-json",
			"created": 1704067200000,
			"messages": [
				{"role": "user", "content": [{"type": "text", "text": "hello"}]}
			]
		}`

		path := createTestFile(t, "bad-name.json", content)
		sess, _, err := parseAmpTestSession(t, path, "local")
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, "amp:T-from-json", sess.ID)
	})

	t.Run("valid JSON id differs from filename", func(t *testing.T) {
		// Both the JSON id and filename are valid T-<id>
		// patterns, but they disagree. Filename must win so
		// FindAmpSourceFile can locate the file by ID.
		content := `{
			"v": 1,
			"id": "T-from-json",
			"created": 1704067200000,
			"messages": [
				{"role": "user", "content": [{"type": "text", "text": "hello"}]}
			]
		}`

		path := createTestFile(t, "T-from-file.json", content)
		sess, _, err := parseAmpTestSession(t, path, "local")
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, "amp:T-from-file", sess.ID)
	})
}

func TestAmpThreadID(t *testing.T) {
	data := []byte(`{"id":"T-abc123","v":1}`)
	assert.Equal(t, "T-abc123", AmpThreadID(data))

	assert.Equal(t, "", AmpThreadID([]byte(`{"v":1}`)))
}
