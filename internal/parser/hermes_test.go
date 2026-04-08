package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runHermesJSONLTest(
	t *testing.T, filename, content string,
) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	if filename == "" {
		filename = "20260403_153620_5a3e2ff1.jsonl"
	}
	path := createTestFile(t, filename, content)
	sess, msgs, err := ParseHermesSession(
		path, "", "local",
	)
	require.NoError(t, err)
	return sess, msgs
}

func runHermesJSONTest(
	t *testing.T, filename, content string,
) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	if filename == "" {
		filename = "session_20260403_153620_5a3e2ff1.json"
	}
	path := createTestFile(t, filename, content)
	sess, msgs, err := ParseHermesSession(
		path, "", "local",
	)
	require.NoError(t, err)
	return sess, msgs
}

// --- JSONL format tests ---

func TestParseHermesSession_JSONL_Basic(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","platform":"linux","model":"gpt-4","timestamp":"2026-04-03T15:27:00.000000"}`,
		`{"role":"user","content":"Fix the tests","timestamp":"2026-04-03T15:27:21.014566"}`,
		`{"role":"assistant","content":"I will fix them now.","timestamp":"2026-04-03T15:27:25.123456"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"hermes:20260403_153620_5a3e2ff1",
		"hermes-linux", AgentHermes,
	)
	assert.Equal(t, "Fix the tests", sess.FirstMessage)
	assertMessageCount(t, sess.MessageCount, 2)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "local", sess.Machine)

	require.Len(t, msgs, 2)
	assertMessage(t, msgs[0], RoleUser, "Fix the tests")
	assertMessage(t, msgs[1], RoleAssistant, "I will fix them now.")
	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.Equal(t, 1, msgs[1].Ordinal)
}

func TestParseHermesSession_JSONL_ToolCalls(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","platform":"darwin","timestamp":"2026-04-03T15:00:00.000000"}`,
		`{"role":"user","content":"Read main.go","timestamp":"2026-04-03T15:00:01.000000"}`,
		`{"role":"assistant","content":"","reasoning":"Let me read it.","finish_reason":"tool_calls","tool_calls":[{"id":"tc1","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}],"timestamp":"2026-04-03T15:00:02.000000"}`,
		`{"role":"tool","content":"package main\n","tool_call_id":"tc1","timestamp":"2026-04-03T15:00:03.000000"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)

	assertMessageCount(t, sess.MessageCount, 3)
	assert.Equal(t, 1, sess.UserMessageCount)

	// Assistant message with reasoning and tool call.
	assert.True(t, msgs[1].HasThinking)
	assert.True(t, msgs[1].HasToolUse)
	assert.Contains(t, msgs[1].Content, "[Thinking]")
	assert.Contains(t, msgs[1].Content, "Let me read it.")
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "read_file", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].Category)
	assert.Equal(t, "tc1", msgs[1].ToolCalls[0].ToolUseID)

	// Tool result message.
	assert.Equal(t, RoleUser, msgs[2].Role)
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "tc1", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(t,
		"package main\n",
		DecodeContent(msgs[2].ToolResults[0].ContentRaw),
	)
}

func TestParseHermesSession_JSONL_MultipleToolCalls(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","timestamp":"2026-04-03T15:00:00.000000"}`,
		`{"role":"user","content":"Check both files","timestamp":"2026-04-03T15:00:01.000000"}`,
		`{"role":"assistant","content":"Reading both.","tool_calls":[{"id":"tc1","function":{"name":"read_file","arguments":"{}"}},{"id":"tc2","function":{"name":"search_files","arguments":"{}"}}],"timestamp":"2026-04-03T15:00:02.000000"}`,
	}, "\n")

	_, msgs := runHermesJSONLTest(t, "", content)
	require.Len(t, msgs[1].ToolCalls, 2)
	assert.Equal(t, "read_file", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].Category)
	assert.Equal(t, "search_files", msgs[1].ToolCalls[1].ToolName)
	assert.Equal(t, "Grep", msgs[1].ToolCalls[1].Category)
}

func TestParseHermesSession_JSONL_NoPlatform(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","model":"gpt-4"}`,
		`{"role":"user","content":"hello","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")

	sess, _ := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	assert.Equal(t, "hermes", sess.Project)
}

func TestParseHermesSession_JSONL_ExplicitProject(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","platform":"darwin"}`,
		`{"role":"user","content":"hello","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")

	path := createTestFile(
		t, "20260403_153620_abc.jsonl", content,
	)
	sess, _, err := ParseHermesSession(
		path, "my-project", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "my-project", sess.Project)
}

func TestParseHermesSession_JSONL_EmptyMessages(t *testing.T) {
	content := `{"role":"session_meta","platform":"linux"}`
	sess, msgs := runHermesJSONLTest(t, "", content)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestParseHermesSession_JSONL_EmptyUserContent(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`{"role":"user","content":"","timestamp":"2026-04-03T15:00:00.000000"}`,
		`{"role":"user","content":"   ","timestamp":"2026-04-03T15:00:01.000000"}`,
		`{"role":"user","content":"real message","timestamp":"2026-04-03T15:00:02.000000"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	assertMessageCount(t, sess.MessageCount, 1)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "real message", sess.FirstMessage)
	require.Len(t, msgs, 1)
}

func TestParseHermesSession_JSONL_EmptyAssistant(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`{"role":"user","content":"hi","timestamp":"2026-04-03T15:00:00.000000"}`,
		`{"role":"assistant","content":"","timestamp":"2026-04-03T15:00:01.000000"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	// Empty assistant with no tool calls is skipped.
	assertMessageCount(t, sess.MessageCount, 1)
	require.Len(t, msgs, 1)
}

func TestParseHermesSession_JSONL_ToolResultNoID(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`{"role":"user","content":"hi","timestamp":"2026-04-03T15:00:00.000000"}`,
		`{"role":"tool","content":"result","tool_call_id":"","timestamp":"2026-04-03T15:00:01.000000"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	// Tool result without ID is skipped.
	assertMessageCount(t, sess.MessageCount, 1)
	require.Len(t, msgs, 1)
}

func TestParseHermesSession_JSONL_InvalidJSON(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`not valid json`,
		`{"role":"user","content":"hello","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")

	sess, msgs := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	// Invalid line is skipped, valid messages are parsed.
	assertMessageCount(t, sess.MessageCount, 1)
	require.Len(t, msgs, 1)
}

func TestParseHermesSession_JSONL_Timestamps(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta","timestamp":"2026-04-03T10:00:00.000000"}`,
		`{"role":"user","content":"first","timestamp":"2026-04-03T10:00:05.000000"}`,
		`{"role":"assistant","content":"reply","timestamp":"2026-04-03T10:05:00.000000"}`,
	}, "\n")

	sess, _ := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)

	wantStart := time.Date(
		2026, 4, 3, 10, 0, 0, 0, time.Local,
	)
	wantEnd := time.Date(
		2026, 4, 3, 10, 5, 0, 0, time.Local,
	)
	assertTimestamp(t, sess.StartedAt, wantStart)
	assertTimestamp(t, sess.EndedAt, wantEnd)
}

func TestParseHermesSession_JSONL_FirstMessageTruncation(t *testing.T) {
	longMsg := strings.Repeat("a", 400)
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`{"role":"user","content":"` + longMsg + `","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")

	sess, _ := runHermesJSONLTest(t, "", content)
	require.NotNil(t, sess)
	// truncate clips at 300 + 3 ellipsis = 303.
	assert.Equal(t, 303, len(sess.FirstMessage))
}

func TestParseHermesSession_JSONL_Errors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, _, err := ParseHermesSession(
			"/nonexistent/file.jsonl", "", "local",
		)
		assert.Error(t, err)
	})
}

// --- JSON format tests ---

func TestParseHermesSession_JSON_Basic(t *testing.T) {
	content := `{
		"platform": "darwin",
		"session_start": "2026-04-03T15:00:00.000000",
		"last_updated": "2026-04-03T15:05:00.000000",
		"messages": [
			{"role": "user", "content": "Deploy the app", "timestamp": "2026-04-03T15:00:01.000000"},
			{"role": "assistant", "content": "Deploying now.", "timestamp": "2026-04-03T15:00:05.000000"}
		]
	}`

	sess, msgs := runHermesJSONTest(t, "", content)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"hermes:20260403_153620_5a3e2ff1",
		"hermes-darwin", AgentHermes,
	)
	assert.Equal(t, "Deploy the app", sess.FirstMessage)
	assertMessageCount(t, sess.MessageCount, 2)
	assert.Equal(t, 1, sess.UserMessageCount)

	require.Len(t, msgs, 2)
	assertMessage(t, msgs[0], RoleUser, "Deploy the app")
	assertMessage(t, msgs[1], RoleAssistant, "Deploying now.")
}

func TestParseHermesSession_JSON_ToolCalls(t *testing.T) {
	content := `{
		"session_start": "2026-04-03T15:00:00.000000",
		"messages": [
			{"role": "user", "content": "Edit the file", "timestamp": "2026-04-03T15:00:01.000000"},
			{
				"role": "assistant",
				"content": "Editing.",
				"reasoning": "I need to patch it.",
				"tool_calls": [
					{"id": "tc1", "function": {"name": "patch", "arguments": "{\"file\":\"main.go\"}"}}
				],
				"timestamp": "2026-04-03T15:00:02.000000"
			},
			{"role": "tool", "content": "patched", "tool_call_id": "tc1", "timestamp": "2026-04-03T15:00:03.000000"}
		]
	}`

	sess, msgs := runHermesJSONTest(t, "", content)
	require.NotNil(t, sess)
	assertMessageCount(t, sess.MessageCount, 3)

	assert.True(t, msgs[1].HasThinking)
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "patch", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Edit", msgs[1].ToolCalls[0].Category)

	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "tc1", msgs[2].ToolResults[0].ToolUseID)
}

func TestParseHermesSession_JSON_ReasoningDetails(t *testing.T) {
	content := `{
		"messages": [
			{"role": "user", "content": "think hard", "timestamp": "2026-04-03T15:00:00.000000"},
			{"role": "assistant", "content": "done", "reasoning_details": "deep thought", "timestamp": "2026-04-03T15:00:01.000000"}
		]
	}`

	_, msgs := runHermesJSONTest(t, "", content)
	// reasoning_details is a fallback for reasoning.
	assert.True(t, msgs[1].HasThinking)
	assert.Contains(t, msgs[1].Content, "deep thought")
}

func TestParseHermesSession_JSON_EmptyMessages(t *testing.T) {
	content := `{"platform":"linux","messages":[]}`
	sess, msgs := runHermesJSONTest(t, "", content)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestParseHermesSession_JSON_NoPlatform(t *testing.T) {
	content := `{
		"messages": [
			{"role": "user", "content": "hi", "timestamp": "2026-04-03T15:00:00.000000"}
		]
	}`
	sess, _ := runHermesJSONTest(t, "", content)
	require.NotNil(t, sess)
	assert.Equal(t, "hermes", sess.Project)
}

func TestParseHermesSession_JSON_MessageTimestampsExtendBounds(
	t *testing.T,
) {
	// Per-message timestamps can extend session bounds beyond
	// the envelope session_start/last_updated.
	content := `{
		"session_start": "2026-04-03T15:00:00.000000",
		"last_updated": "2026-04-03T15:05:00.000000",
		"messages": [
			{"role": "user", "content": "early", "timestamp": "2026-04-03T14:50:00.000000"},
			{"role": "assistant", "content": "late", "timestamp": "2026-04-03T15:10:00.000000"}
		]
	}`

	sess, _ := runHermesJSONTest(t, "", content)
	require.NotNil(t, sess)

	wantStart := time.Date(
		2026, 4, 3, 14, 50, 0, 0, time.Local,
	)
	wantEnd := time.Date(
		2026, 4, 3, 15, 10, 0, 0, time.Local,
	)
	assertTimestamp(t, sess.StartedAt, wantStart)
	assertTimestamp(t, sess.EndedAt, wantEnd)
}

func TestParseHermesSession_JSON_Errors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, _, err := ParseHermesSession(
			"/nonexistent/file.json", "", "local",
		)
		assert.Error(t, err)
	})

	t.Run("not an object", func(t *testing.T) {
		path := createTestFile(
			t, "session_bad.json", `"just a string"`,
		)
		_, _, err := ParseHermesSession(path, "", "local")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid JSON")
	})
}

// --- Routing test ---

func TestParseHermesSession_RoutesOnExtension(t *testing.T) {
	jsonlContent := strings.Join([]string{
		`{"role":"session_meta","platform":"linux"}`,
		`{"role":"user","content":"jsonl path","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")
	jsonContent := `{
		"platform": "darwin",
		"messages": [
			{"role":"user","content":"json path","timestamp":"2026-04-03T15:00:00.000000"}
		]
	}`

	t.Run("jsonl", func(t *testing.T) {
		sess, _ := runHermesJSONLTest(
			t, "20260403_test.jsonl", jsonlContent,
		)
		require.NotNil(t, sess)
		assert.Equal(t, "hermes-linux", sess.Project)
	})

	t.Run("json", func(t *testing.T) {
		sess, _ := runHermesJSONTest(
			t, "session_test.json", jsonContent,
		)
		require.NotNil(t, sess)
		assert.Equal(t, "hermes-darwin", sess.Project)
	})
}

// --- HermesSessionID ---

func TestHermesSessionID(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"20260403_153620_5a3e2ff1.jsonl", "20260403_153620_5a3e2ff1"},
		{"20260403_153620_5a3e2ff1.json", "20260403_153620_5a3e2ff1"},
		{"session_20260403_abc.json", "20260403_abc"},
		{"session_20260403_abc.jsonl", "20260403_abc"},
		{"plain_name.jsonl", "plain_name"},
		{"no_ext", "no_ext"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HermesSessionID(tt.name)
			if got != tt.want {
				t.Errorf(
					"HermesSessionID(%q) = %q, want %q",
					tt.name, got, tt.want,
				)
			}
		})
	}
}

// --- parseHermesTimestamp ---

func TestParseHermesTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  time.Time
	}{
		{
			"microseconds",
			"2026-04-03T15:27:21.014566",
			time.Date(
				2026, 4, 3, 15, 27, 21, 14566000,
				time.Local,
			),
		},
		{
			"no fractional seconds",
			"2026-04-03T15:27:21",
			time.Date(
				2026, 4, 3, 15, 27, 21, 0,
				time.Local,
			),
		},
		{
			"RFC3339 with timezone",
			"2026-04-03T15:27:21Z",
			time.Date(
				2026, 4, 3, 15, 27, 21, 0, time.UTC,
			),
		},
		{
			"RFC3339 with offset",
			"2026-04-03T15:27:21+05:30",
			time.Date(
				2026, 4, 3, 15, 27, 21, 0,
				time.FixedZone("", 5*3600+30*60),
			),
		},
		{
			"empty string",
			"",
			time.Time{},
		},
		{
			"garbage",
			"not-a-timestamp",
			time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHermesTimestamp(tt.input)
			if tt.want.IsZero() {
				assertZeroTimestamp(t, got, "timestamp")
			} else {
				assertTimestamp(t, got, tt.want)
			}
		})
	}
}

// --- stripHermesSkillPrefix ---

func TestStripHermesSkillPrefix(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			"no prefix",
			"Fix the bug in main.go",
			"Fix the bug in main.go",
		},
		{
			"skill with user instruction",
			`[SYSTEM: The user has invoked the "commit" skill. Please follow the instructions below.]` +
				"\n\n---\nname: commit\n---\ncommit stuff\n\n" +
				"The user has provided the following instruction alongside the skill invocation: Please commit my changes",
			"Please commit my changes",
		},
		{
			"skill without user instruction",
			`[SYSTEM: The user has invoked the "review" skill. Please follow the instructions.]` +
				"\n\n---\nname: review\n---\nreview stuff\n\n",
			"[Skill: review]",
		},
		{
			"skill with runtime note stripped",
			`[SYSTEM: The user has invoked the "debug" skill. Follow instructions.]` +
				"\n\nThe user has provided the following instruction alongside the skill invocation: Fix it" +
				"\n\n[Runtime note: some internal detail]",
			"Fix it",
		},
		{
			"empty user instruction falls back to skill name",
			`[SYSTEM: The user has invoked the "test" skill. Follow instructions.]` +
				"\n\nThe user has provided the following instruction alongside the skill invocation:   ",
			"[Skill: test]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripHermesSkillPrefix(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- DiscoverHermesSessions ---

func TestDiscoverHermesSessions(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		wantFiles []string
	}{
		{
			name: "JSONL only",
			files: map[string]string{
				"20260403_153620_aaa.jsonl": "{}",
				"20260404_100000_bbb.jsonl": "{}",
			},
			wantFiles: []string{
				"20260403_153620_aaa.jsonl",
				"20260404_100000_bbb.jsonl",
			},
		},
		{
			name: "JSON only",
			files: map[string]string{
				"session_20260403_aaa.json": "{}",
			},
			wantFiles: []string{
				"session_20260403_aaa.json",
			},
		},
		{
			name: "JSONL takes priority over JSON",
			files: map[string]string{
				"20260403_153620_aaa.jsonl":  "{}",
				"session_20260403_153620_aaa.json": "{}",
			},
			wantFiles: []string{
				"20260403_153620_aaa.jsonl",
			},
		},
		{
			name: "non-session JSON ignored",
			files: map[string]string{
				"config.json":    "{}",
				"random.json":    "{}",
				"notes.txt":      "hi",
				"session_a.json": "{}",
			},
			wantFiles: []string{
				"session_a.json",
			},
		},
		{
			name: "directories ignored",
			files: map[string]string{
				"20260403_aaa.jsonl":      "{}",
				"subdir/nested.jsonl":     "{}",
			},
			wantFiles: []string{
				"20260403_aaa.jsonl",
			},
		},
		{
			name:      "empty dir",
			files:     map[string]string{},
			wantFiles: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)
			files := DiscoverHermesSessions(dir)
			assertDiscoveredFiles(
				t, files, tt.wantFiles, AgentHermes,
			)
		})
	}

	t.Run("empty string dir", func(t *testing.T) {
		files := DiscoverHermesSessions("")
		assert.Nil(t, files)
	})

	t.Run("nonexistent dir", func(t *testing.T) {
		files := DiscoverHermesSessions(
			filepath.Join(t.TempDir(), "nope"),
		)
		assert.Nil(t, files)
	})
}

// --- FindHermesSourceFile ---

func TestFindHermesSourceFile(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		sessionID string
		wantFile  string
	}{
		{
			name:      "finds JSONL",
			files:     map[string]string{"20260403_aaa.jsonl": "{}"},
			sessionID: "20260403_aaa",
			wantFile:  "20260403_aaa.jsonl",
		},
		{
			name:      "finds JSON",
			files:     map[string]string{"session_20260403_aaa.json": "{}"},
			sessionID: "20260403_aaa",
			wantFile:  "session_20260403_aaa.json",
		},
		{
			name: "prefers JSONL over JSON",
			files: map[string]string{
				"20260403_aaa.jsonl":      "{}",
				"session_20260403_aaa.json": "{}",
			},
			sessionID: "20260403_aaa",
			wantFile:  "20260403_aaa.jsonl",
		},
		{
			name:      "not found",
			files:     map[string]string{"20260403_aaa.jsonl": "{}"},
			sessionID: "nonexistent",
			wantFile:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setupFileSystem(t, dir, tt.files)

			got := FindHermesSourceFile(dir, tt.sessionID)
			want := ""
			if tt.wantFile != "" {
				want = filepath.Join(dir, tt.wantFile)
			}
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}

	t.Run("invalid session IDs", func(t *testing.T) {
		dir := t.TempDir()
		setupFileSystem(t, dir, map[string]string{
			"20260403_aaa.jsonl": "{}",
		})
		for _, id := range []string{"", "../etc/passwd", "a/b", "a b"} {
			got := FindHermesSourceFile(dir, id)
			if got != "" {
				t.Errorf(
					"FindHermesSourceFile(%q) = %q, want empty",
					id, got,
				)
			}
		}
	})
}

// --- Taxonomy ---

func TestHermesToolTaxonomy(t *testing.T) {
	tests := []struct {
		tool     string
		category string
	}{
		{"read_file", "Read"},
		{"write_file", "Write"},
		{"edit_file", "Edit"},
		{"search_files", "Grep"},
		{"run_command", "Bash"},
		{"execute_command", "Bash"},
		{"patch", "Edit"},
		{"terminal", "Bash"},
		{"execute_code", "Bash"},
		{"vision_analyze", "Read"},
		{"delegate_task", "Task"},
		{"browser_navigate", "Tool"},
		{"browser_click", "Tool"},
		{"todo", "Tool"},
		{"memory", "Tool"},
		{"skill_view", "Tool"},
	}

	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			got := NormalizeToolCategory(tt.tool)
			if got != tt.category {
				t.Errorf(
					"NormalizeToolCategory(%q) = %q, want %q",
					tt.tool, got, tt.category,
				)
			}
		})
	}
}

// --- Registry ---

func TestHermesRegistryEntry(t *testing.T) {
	var found *AgentDef
	for i := range Registry {
		if Registry[i].Type == AgentHermes {
			found = &Registry[i]
			break
		}
	}
	require.NotNil(t, found, "AgentHermes not in Registry")

	assert.Equal(t, "Hermes Agent", found.DisplayName)
	assert.Equal(t, "HERMES_SESSIONS_DIR", found.EnvVar)
	assert.Equal(t, "hermes_sessions_dirs", found.ConfigKey)
	assert.Equal(t, "hermes:", found.IDPrefix)
	assert.True(t, found.FileBased)
	assert.Contains(t, found.DefaultDirs, ".hermes/sessions")
	assert.NotNil(t, found.DiscoverFunc)
	assert.NotNil(t, found.FindSourceFunc)
}

// --- File info ---

func TestParseHermesSession_FileInfo(t *testing.T) {
	content := strings.Join([]string{
		`{"role":"session_meta"}`,
		`{"role":"user","content":"hi","timestamp":"2026-04-03T15:00:00.000000"}`,
	}, "\n")

	path := createTestFile(
		t, "20260403_test.jsonl", content,
	)
	info, err := os.Stat(path)
	require.NoError(t, err)

	sess, _, err := ParseHermesSession(path, "", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, path, sess.File.Path)
	assert.Equal(t, info.Size(), sess.File.Size)
	assert.Equal(t, info.ModTime().UnixNano(), sess.File.Mtime)
}
