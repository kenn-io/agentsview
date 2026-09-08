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

func cursorTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed
}

func parseCursorTestFile(t *testing.T, path string) (*ParsedSession, []ParsedMessage) {
	t.Helper()
	provider := &cursorProvider{}
	session, messages, err := provider.parseSession(
		path, "project", "/workspace/project", "test-machine",
	)
	require.NoError(t, err)
	return session, messages
}

func TestCursorLegacyTimestampReproduction(t *testing.T) {
	data, err := os.ReadFile("testdata/cursor/legacy-timestamps.txt")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "legacy-timestamps.txt")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	session, messages := parseCursorTestFile(t, path)
	require.Len(t, messages, 4)
	assert.Equal(t, cursorTestTime(t, "2026-07-02T15:11:00Z"), messages[0].Timestamp)
	assert.Equal(t, "Inspect the repository.", messages[0].Content)
	assert.Zero(t, messages[1].Timestamp)
	assert.Equal(t, cursorTestTime(t, "2026-07-02T15:12:00Z"), messages[2].Timestamp)
	assert.Equal(t, "Continue with the next step.", messages[2].Content)
	assert.Zero(t, messages[3].Timestamp)
	assert.Equal(t, messages[0].Timestamp, session.StartedAt)
	assert.Equal(t, messages[2].Timestamp, session.EndedAt)
	t.Logf("turn 1 = %s; turn 2 = %s; assistant_timestamp_zero=%t", messages[0].Timestamp.Format(time.RFC3339), messages[2].Timestamp.Format(time.RFC3339), messages[1].Timestamp.IsZero())
}

func TestCursorJSONLTimestampFromTextBlock(t *testing.T) {
	data := strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Tuesday, Jun 16, 2026, 8:41 PM (UTC-4)</timestamp>\n<user_query>Inspect the logs.</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"The logs are clean."}]}}`,
	}, "\n")
	messages := parseCursorJSONL(data)
	require.Len(t, messages, 2)
	assert.Equal(t, cursorTestTime(t, "2026-06-17T00:41:00Z"), messages[0].Timestamp)
	assert.Equal(t, "Inspect the logs.", messages[0].Content)
	assert.Zero(t, messages[1].Timestamp)
}

func TestCursorTimestampDayBoundary(t *testing.T) {
	late, ok := parseCursorTimestamp(
		"<timestamp>Thursday, Jul 2, 2026, 11:59 PM (UTC-4)</timestamp>",
	)
	require.True(t, ok)
	early, ok := parseCursorTimestamp(
		"<timestamp>Friday, Jul 3, 2026, 12:00 AM (UTC-4)</timestamp>",
	)
	require.True(t, ok)
	assert.True(t, late.Before(early))
	assert.Zero(t, late.Second())
	assert.Zero(t, late.Nanosecond())
	assert.Zero(t, early.Second())
	assert.Zero(t, early.Nanosecond())
}

func TestCursorTimestampNoonMidnight(t *testing.T) {
	midnight, ok := parseCursorTimestamp(
		"<timestamp>Thursday, Jul 2, 2026, 12:00 AM (UTC-4)</timestamp>",
	)
	require.True(t, ok)
	noon, ok := parseCursorTimestamp(
		"<timestamp>Thursday, Jul 2, 2026, 12:00 PM (UTC-4)</timestamp>",
	)
	require.True(t, ok)
	assert.Equal(t, cursorTestTime(t, "2026-07-02T04:00:00Z"), midnight)
	assert.Equal(t, cursorTestTime(t, "2026-07-02T16:00:00Z"), noon)
}

func TestCursorTimestampExplicitOffsets(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want string
	}{
		{
			name: "observed offset",
			tag:  "<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)</timestamp>",
			want: "2026-07-02T15:11:00Z",
		},
		{
			name: "half hour offset",
			tag:  "<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC+5:30)</timestamp>",
			want: "2026-07-02T05:41:00Z",
		},
		{
			name: "zero offset",
			tag:  "<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC+0)</timestamp>",
			want: "2026-07-02T11:11:00Z",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCursorTimestamp(tt.tag)
			require.True(t, ok)
			assert.Equal(t, cursorTestTime(t, tt.want), got)
		})
	}
	_, ok := parseCursorTimestamp(
		"<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC)</timestamp>",
	)
	assert.False(t, ok)
}

func TestCursorMalformedTimestampTag(t *testing.T) {
	fallback := cursorTestTime(t, "2026-01-01T00:00:00Z")
	tests := []struct {
		name        string
		text        string
		wantContent string
	}{
		{
			name:        "invalid date",
			text:        "user:\n<timestamp>Thursday, Feb 30, 2026, 11:11 AM (UTC-4)</timestamp>\n<user_query>Keep this.</user_query>",
			wantContent: "Keep this.",
		},
		{
			name:        "incomplete tag",
			text:        "user:\n<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)\n<user_query>Keep this.</user_query>",
			wantContent: "Keep this.",
		},
		{
			name:        "unsupported bare UTC",
			text:        "user:\n<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC)</timestamp>\n<user_query>Keep this.</user_query>",
			wantContent: "Keep this.",
		},
		{
			name:        "tag without query",
			text:        "user:\n<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)</timestamp>\nKeep this.",
			wantContent: "<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)</timestamp>\nKeep this.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "malformed.txt")
			require.NoError(t, os.WriteFile(path, []byte(tt.text), 0o644))
			require.NoError(t, os.Chtimes(path, fallback, fallback))
			session, messages := parseCursorTestFile(t, path)
			require.Len(t, messages, 1)
			assert.Zero(t, messages[0].Timestamp)
			assert.Equal(t, tt.wantContent, messages[0].Content)
			assert.True(t, session.StartedAt.Equal(fallback))
			assert.True(t, session.EndedAt.Equal(fallback))
		})
	}
}

func TestCursorTimestampRequiresLeadingCompleteUserQuery(t *testing.T) {
	lines := []string{
		"<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)</timestamp>",
		"ordinary text before the query",
		"<user_query>Keep the baseline extraction.</user_query>",
	}

	content, timestamp := extractCursorUserContent(lines)
	assert.Zero(t, timestamp)
	assert.Equal(t, "Keep the baseline extraction.", content)
}

func TestCursorLiteralTimestampXML(t *testing.T) {
	text := strings.Join([]string{
		"user:",
		"<user_query>Show the literal <timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)</timestamp> text.</user_query>",
		"assistant:",
		"The assistant repeats <timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)</timestamp>.",
	}, "\n")
	messages := parseCursorMessages(strings.Split(text, "\n"))
	require.Len(t, messages, 2)
	assert.Contains(t, messages[0].Content, "<timestamp>")
	assert.Contains(t, messages[1].Content, "<timestamp>")
	assert.Zero(t, messages[0].Timestamp)
	assert.Zero(t, messages[1].Timestamp)
}

func TestCursorSessionBoundsFromTurns(t *testing.T) {
	text := strings.Join([]string{
		"user:", "<timestamp>Thursday, Jul 2, 2026, 11:59 PM (UTC-4)</timestamp>", "<user_query>late</user_query>",
		"assistant:", "answer",
		"user:", "<timestamp>Friday, Jul 3, 2026, 12:00 AM (UTC-4)</timestamp>", "<user_query>early</user_query>",
	}, "\n")
	path := filepath.Join(t.TempDir(), "bounds.txt")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o644))
	session, messages := parseCursorTestFile(t, path)
	require.Len(t, messages, 3)
	assert.Equal(t, cursorTestTime(t, "2026-07-03T03:59:00Z"), session.StartedAt)
	assert.Equal(t, cursorTestTime(t, "2026-07-03T04:00:00Z"), session.EndedAt)
	assert.Equal(t, RoleUser, messages[0].Role)
	assert.Equal(t, RoleAssistant, messages[1].Role)
	assert.Equal(t, RoleUser, messages[2].Role)
}

func TestCursorMixedTaggedUntagged(t *testing.T) {
	text := "user:\n<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)</timestamp>\n<user_query>tagged</user_query>\n" +
		"assistant:\nanswer\nuser:\n<user_query>untagged</user_query>\n"
	path := filepath.Join(t.TempDir(), "mixed.txt")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o644))
	_, messages := parseCursorTestFile(t, path)
	require.Len(t, messages, 3)
	assert.False(t, messages[0].Timestamp.IsZero())
	assert.Zero(t, messages[1].Timestamp)
	assert.Zero(t, messages[2].Timestamp)
}

func TestCursorIdenticalContentDifferentMtimeSameTimes(t *testing.T) {
	text := "user:\n<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)</timestamp>\n<user_query>same</user_query>\n"
	firstPath := filepath.Join(t.TempDir(), "first.txt")
	secondPath := filepath.Join(t.TempDir(), "second.txt")
	require.NoError(t, os.WriteFile(firstPath, []byte(text), 0o644))
	require.NoError(t, os.WriteFile(secondPath, []byte(text), 0o644))
	firstMtime := cursorTestTime(t, "2026-01-01T00:00:00Z")
	secondMtime := cursorTestTime(t, "2026-02-01T00:00:00Z")
	require.NoError(t, os.Chtimes(firstPath, firstMtime, firstMtime))
	require.NoError(t, os.Chtimes(secondPath, secondMtime, secondMtime))
	firstSession, firstMessages := parseCursorTestFile(t, firstPath)
	secondSession, secondMessages := parseCursorTestFile(t, secondPath)
	assert.NotEqual(t, firstSession.File.Mtime, secondSession.File.Mtime)
	assert.Equal(t, firstMessages[0].Timestamp, secondMessages[0].Timestamp)
	assert.Equal(t, firstSession.StartedAt, secondSession.StartedAt)
	assert.Equal(t, firstSession.EndedAt, secondSession.EndedAt)
}

func TestCursorParentAndChildTimestamps(t *testing.T) {
	root := t.TempDir()
	parentPath := filepath.Join(root, "parent.txt")
	childPath := filepath.Join(root, "child.txt")
	require.NoError(t, os.WriteFile(parentPath, []byte("user:\n<timestamp>Thursday, Jul 2, 2026, 11:11 AM (UTC-4)</timestamp>\n<user_query>parent</user_query>\n"), 0o644))
	require.NoError(t, os.WriteFile(childPath, []byte("user:\n<timestamp>Thursday, Jul 2, 2026, 11:12 AM (UTC-4)</timestamp>\n<user_query>child</user_query>\n"), 0o644))
	parent, parentMessages := parseCursorTestFile(t, parentPath)
	child, childMessages := parseCursorTestFile(t, childPath)
	require.Len(t, parentMessages, 1)
	require.Len(t, childMessages, 1)
	assert.Equal(t, parentMessages[0].Timestamp, parent.StartedAt)
	assert.Equal(t, childMessages[0].Timestamp, child.StartedAt)
	assert.NotEqual(t, parent.StartedAt, child.StartedAt)
}

func TestExtractAssistantContent(t *testing.T) {
	tests := []struct {
		name          string
		lines         []string
		wantText      string
		wantThinking  bool
		wantToolCount int
	}{
		{
			name: "plain text",
			lines: []string{
				"Hello, how can I help?",
			},
			wantText: "Hello, how can I help?",
		},
		{
			name: "thinking then text",
			lines: []string{
				"[Thinking]",
				"  internal reasoning...",
				"Here is my answer.",
			},
			wantText:     "Here is my answer.",
			wantThinking: true,
		},
		{
			name: "tool call then prose",
			lines: []string{
				"[Tool call] EditFile",
				"  path=/tmp/foo.go",
				"  content=bar",
				"I edited the file for you.",
			},
			wantText:      "I edited the file for you.",
			wantToolCount: 1,
		},
		{
			name: "tool result then prose",
			lines: []string{
				"[Tool result]",
				"  success: true",
				"The operation completed.",
			},
			wantText: "The operation completed.",
		},
		{
			name: "thinking, tool, prose sequence",
			lines: []string{
				"[Thinking]",
				"  let me think...",
				"[Tool call] Shell",
				"  command=ls",
				"[Tool result]",
				"  file1.go",
				"Here are the files I found.",
			},
			wantText:      "Here are the files I found.",
			wantThinking:  true,
			wantToolCount: 1,
		},
		{
			name: "prose between markers",
			lines: []string{
				"First I'll check the file.",
				"[Tool call] ReadFile",
				"  path=main.go",
				"[Tool result]",
				"  package main",
				"The file looks good.",
				"[Tool call] Shell",
				"  command=go build",
				"Build succeeded.",
			},
			wantText: "First I'll check the file.\n" +
				"The file looks good.\n" +
				"Build succeeded.",
			wantToolCount: 2,
		},
		{
			name:  "empty lines",
			lines: []string{},
		},
		{
			name: "only markers no prose",
			lines: []string{
				"[Thinking]",
				"  reasoning",
				"[Tool call] Shell",
				"  command=echo hi",
			},
			wantThinking:  true,
			wantToolCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, hasThinking, toolCalls := extractAssistantContent(
				tt.lines,
			)
			assert.Equal(t, tt.wantText, text, "text")
			assert.Equal(t, tt.wantThinking, hasThinking, "hasThinking")
			assert.Len(t, toolCalls, tt.wantToolCount, "tool call count")
		})
	}
}

func TestExtractAssistantContentCursorApplyPatch(t *testing.T) {
	patch := "@@ -1,1 +1,1 @@\n-old\n+new"
	lines := []string{
		"[Tool call] ApplyPatch",
		`  {"patch":"` + strings.ReplaceAll(patch, "\n", `\n`) + `","path":"src/app.ts"}`,
	}

	text, hasThinking, toolCalls := extractAssistantContent(lines)

	assert.Empty(t, text)
	assert.False(t, hasThinking)
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "ApplyPatch", toolCalls[0].ToolName)
	assert.Equal(t, "Edit", toolCalls[0].Category)
	assert.JSONEq(t,
		`{"patch":"@@ -1,1 +1,1 @@\n-old\n+new","path":"src/app.ts"}`,
		toolCalls[0].InputJSON)
}

func TestExtractAssistantContentCursorApplyPatchDedentsRawPatch(t *testing.T) {
	lines := []string{
		"[Tool call] ApplyPatch",
		"  @@ -1,1 +1,1 @@",
		"  -old",
		"  +new",
	}

	_, _, toolCalls := extractAssistantContent(lines)

	require.Len(t, toolCalls, 1)
	assert.JSONEq(t,
		`{"patch":"@@ -1,1 +1,1 @@\n-old\n+new"}`,
		toolCalls[0].InputJSON)
}

func TestIsContainedIn_EdgeCases(t *testing.T) {
	// isContainedIn is in sync/discovery.go; we test
	// isBlockBodyEnd here since it's in cursor.go.
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"marker", "[Tool call] Shell", true},
		{"indented", "  param=value", false},
		{"tab indented", "\tparam=value", false},
		{"empty", "", false},
		{"left-margin prose", "Here is text.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBlockBodyEnd(tt.line)
			assert.Equal(t, tt.want, got, "isBlockBodyEnd(%q)", tt.line)
		})
	}
}

func TestCursorSessionID(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/a/b/abc123.txt", "cursor:abc123"},
		{"/a/b/abc123.jsonl", "cursor:abc123"},
		{"/a/b/no-ext", "cursor:no-ext"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := CursorSessionID(tt.path)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsCursorJSONL(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			"valid jsonl",
			`{"role":"user","message":{"content":"hi"}}`,
			true,
		},
		{
			"leading empty lines",
			"\n\n" + `{"role":"user","message":{"content":"hi"}}`,
			true,
		},
		{
			"many leading blank lines",
			strings.Repeat("\n", 50) +
				`{"role":"user","message":{"content":"hi"}}`,
			true,
		},
		{
			"plain text",
			"user:\nhello\nassistant:\nworld",
			false,
		},
		{
			"empty",
			"",
			false,
		},
		{
			"only blank lines within scan limit",
			strings.Repeat("\n", 100),
			false,
		},
		{
			"first line exceeds 4KB",
			`{"role":"user","message":{"content":"` +
				strings.Repeat("x", 5000) + `"}}`,
			true,
		},
		{
			"first non-empty line beyond 4KB of blanks",
			strings.Repeat("\n", 5000) +
				`{"role":"user","message":{"content":"hi"}}`,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCursorJSONL(tt.data)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseCursorJSONL(t *testing.T) {
	tests := []struct {
		name             string
		lines            []string
		wantCount        int
		wantFirstRole    RoleType
		wantFirstContent string
		wantThinking     bool
		wantToolUse      bool
		wantToolCount    int
	}{
		{
			name: "simple exchange",
			lines: []string{
				`{"role":"user","message":{"content":"Hello"}}`,
				`{"role":"assistant","message":{"content":"Hi there"}}`,
			},
			wantCount:        2,
			wantFirstRole:    RoleUser,
			wantFirstContent: "Hello",
		},
		{
			name: "user with user_query tags",
			lines: []string{
				`{"role":"user","message":{"content":"<user_query>What is Go?</user_query>"}}`,
				`{"role":"assistant","message":{"content":"A programming language."}}`,
			},
			wantCount:        2,
			wantFirstRole:    RoleUser,
			wantFirstContent: "What is Go?",
		},
		{
			name: "content array with text blocks",
			lines: []string{
				`{"role":"user","message":{"content":[{"type":"text","text":"Hello"}]}}`,
				`{"role":"assistant","message":{"content":[{"type":"text","text":"World"}]}}`,
			},
			wantCount:        2,
			wantFirstRole:    RoleUser,
			wantFirstContent: "Hello",
		},
		{
			name: "assistant with tool_use",
			lines: []string{
				`{"role":"user","message":{"content":"Fix it"}}`,
				`{"role":"assistant","message":{"content":[{"type":"text","text":"Let me fix that."},{"type":"tool_use","id":"tu_1","name":"Edit","input":{"file_path":"main.go"}}]}}`,
			},
			wantCount:        2,
			wantFirstRole:    RoleUser,
			wantFirstContent: "Fix it",
			wantToolUse:      true,
			wantToolCount:    1,
		},
		{
			name: "assistant with Cursor ApplyPatch",
			lines: []string{
				`{"role":"user","message":{"content":"Patch it"}}`,
				`{"role":"assistant","message":{"content":[{"type":"tool_use","id":"tu_patch","name":"ApplyPatch","input":{"patch":"@@ -1,1 +1,1 @@\n-old\n+new","path":"src/app.ts"}}]}}`,
			},
			wantCount:        2,
			wantFirstRole:    RoleUser,
			wantFirstContent: "Patch it",
			wantToolUse:      true,
			wantToolCount:    1,
		},
		{
			name: "assistant with thinking",
			lines: []string{
				`{"role":"user","message":{"content":"Explain"}}`,
				`{"role":"assistant","message":{"content":[{"type":"thinking","thinking":"Let me reason..."},{"type":"text","text":"Here is my answer."}]}}`,
			},
			wantCount:        2,
			wantFirstRole:    RoleUser,
			wantFirstContent: "Explain",
			wantThinking:     true,
		},
		{
			name: "mixed thinking, tool_use, text",
			lines: []string{
				`{"role":"user","message":{"content":"Do something"}}`,
				`{"role":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"tool_use","id":"tu_2","name":"Bash","input":{"command":"ls"}},{"type":"text","text":"Done."}]}}`,
			},
			wantCount:        2,
			wantFirstRole:    RoleUser,
			wantFirstContent: "Do something",
			wantThinking:     true,
			wantToolUse:      true,
			wantToolCount:    1,
		},
		{
			name: "skip empty and invalid lines",
			lines: []string{
				"",
				"not json",
				`{"role":"user","message":{"content":"Valid"}}`,
				"",
				`{"role":"assistant","message":{"content":"Also valid"}}`,
			},
			wantCount:        2,
			wantFirstRole:    RoleUser,
			wantFirstContent: "Valid",
		},
		{
			name: "skip unknown role",
			lines: []string{
				`{"role":"system","message":{"content":"System prompt"}}`,
				`{"role":"user","message":{"content":"Question"}}`,
			},
			wantCount:        1,
			wantFirstRole:    RoleUser,
			wantFirstContent: "Question",
		},
		{
			name: "skip message with no content",
			lines: []string{
				`{"role":"user","message":{}}`,
				`{"role":"user","message":{"content":"Real"}}`,
			},
			wantCount:        1,
			wantFirstRole:    RoleUser,
			wantFirstContent: "Real",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := strings.Join(tt.lines, "\n")
			msgs := parseCursorJSONL(data)

			require.Len(t, msgs, tt.wantCount, "message count")
			if tt.wantCount == 0 {
				return
			}

			first := msgs[0]
			assert.Equal(t, tt.wantFirstRole, first.Role, "first role")
			if tt.wantFirstContent != "" {
				assert.Equal(t, tt.wantFirstContent, first.Content, "first content")
			}

			// Check assistant properties on last message
			if tt.wantThinking || tt.wantToolUse ||
				tt.wantToolCount > 0 {
				last := msgs[len(msgs)-1]
				assert.Equal(t, tt.wantThinking, last.HasThinking, "hasThinking")
				assert.Equal(t, tt.wantToolUse, last.HasToolUse, "hasToolUse")
				assert.Len(t, last.ToolCalls, tt.wantToolCount, "tool count")
			}

			// Verify ordinals are contiguous
			for i, m := range msgs {
				assert.Equal(t, i, m.Ordinal, "ordinal[%d]", i)
			}
		})
	}
}

func TestDecodeCursorProjectDir(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{
			"Users-fiona-Documents-project",
			"project",
		},
		{
			"Users-fiona-Documents-my-app",
			"my_app",
		},
		// Marker words inside project names must not
		// cause truncation.
		{
			"Users-wesm-code-my-dev-tool",
			"my_dev_tool",
		},
		{
			"Users-wesm-code-work-bench",
			"work_bench",
		},
		{
			"Users-wesm-code-code-gen",
			"code_gen",
		},
		// Linux home prefix
		{
			"home-user-projects-my-app",
			"my_app",
		},
		// Windows prefix (drive letter)
		{
			"C-Users-user-dev-my-project",
			"my_project",
		},
		// Multi-token usernames
		{
			"Users-john-doe-Documents-my-app",
			"my_app",
		},
		{
			"home-john-doe-projects-my-super-app",
			"my_super_app",
		},
		{
			"C-Users-jane-smith-dev-project",
			"project",
		},
		// Username contains a low-confidence marker word;
		// high-confidence marker later takes precedence.
		{
			"Users-john-code-doe-Documents-my-app",
			"my_app",
		},
		{
			"C-Users-jane-dev-smith-projects-my-app",
			"my_app",
		},
		// Ambiguous: "dev" could be a directory or part
		// of the username. High-confidence "Documents"
		// wins because marker-word usernames are rarer
		// than real ~/Documents paths.
		{
			"Users-john-dev-my-Documents-app",
			"app",
		},
		// No recognized root — fallback to last two
		{
			"opt-builds-my-project",
			"my_project",
		},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := DecodeCursorProjectDir(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}
