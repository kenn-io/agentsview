package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runZencoderParserTest(
	t *testing.T, content string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()
	path := createTestFile(t, "test-uuid.jsonl", content)
	return parseZencoderTestSession(t, path, "local")
}

func parseZencoderTestSession(
	t *testing.T,
	path string,
	machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()

	provider, ok := NewProvider(AgentZencoder, ProviderConfig{
		Roots:   []string{filepath.Dir(path)},
		Machine: machine,
	})
	require.True(t, ok)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: SourceRef{
			Provider:       AgentZencoder,
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

func TestZencoderProviderParsesBasic(t *testing.T) {
	header := `{"id":"abc-123","chatId":"chat-1","modelId":"model-1","parentId":"","creationReason":"newChat","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z","version":"1"}`
	system := `{"role":"system","content":"You are an AI assistant.\n\n# Environment\n\nWorking directory: /home/user/myproject\n\nOS: linux"}`
	user := `{"role":"user","content":[{"type":"text","text":"Fix the bug.","tag":"user-input"}]}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"Sure, I will fix it."}]}`
	finish := `{"role":"finish","reason":"endTurn"}`

	content := strings.Join([]string{
		header, system, user, assistant, finish,
	}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"zencoder:abc-123",
		"myproject", AgentZencoder,
	)

	assert.Equal(t, "Fix the bug.", sess.FirstMessage)
	// System message + user + assistant + finish = 4
	assertMessageCount(t, sess.MessageCount, 4)
	assert.Equal(t, 1, sess.UserMessageCount)

	wantStart := mustParseTime(t, "2024-01-01T00:00:00Z")
	assertTimestamp(t, sess.StartedAt, wantStart)

	wantEnd := mustParseTime(t, "2024-01-01T00:01:00Z")
	assertTimestamp(t, sess.EndedAt, wantEnd)

	require.Equal(t, 4, len(msgs))
	// msg[0]: system message (IsSystem=true)
	assert.True(t, msgs[0].IsSystem)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "Working directory")
	// msg[1]: user message
	assertMessage(t, msgs[1], RoleUser, "Fix the bug.")
	assert.False(t, msgs[1].IsSystem)
	// msg[2]: assistant message
	assertMessage(t, msgs[2], RoleAssistant, "Sure, I will fix it.")
	assert.False(t, msgs[2].IsSystem)
	// msg[3]: finish message (IsSystem=true)
	assert.True(t, msgs[3].IsSystem)
	assert.Equal(t, "[Turn finished: endTurn]", msgs[3].Content)

	// No parent -> no relationship.
	assert.Empty(t, sess.ParentSessionID)
	assert.Equal(t, RelNone, sess.RelationshipType)
}

func TestZencoderProviderParsesToolCallAndReasoning(t *testing.T) {
	header := `{"id":"tc-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Read the file."}]}`
	assistant := `{"role":"assistant","content":[` +
		`{"type":"reasoning","text":"Let me think about this.","provider":"anthropic","subtype":"thinking"},` +
		`{"type":"text","text":"I will read it now."},` +
		`{"type":"tool-call","toolCallId":"tc1","toolName":"Read","input":{"file_path":"main.go"}}` +
		`]}`

	content := strings.Join([]string{
		header, user, assistant,
	}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertMessageCount(t, sess.MessageCount, 2)
	assert.Equal(t, 1, sess.UserMessageCount)

	assert.False(t, msgs[0].HasThinking)
	assert.False(t, msgs[0].HasToolUse)

	assert.True(t, msgs[1].HasThinking)
	assert.True(t, msgs[1].HasToolUse)
	assert.Contains(t, msgs[1].Content, "[Thinking]")
	assert.Contains(t, msgs[1].Content, "Let me think about this.")
	assert.Contains(t, msgs[1].Content, "[Read: main.go]")

	require.Equal(t, 1, len(msgs[1].ToolCalls))
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].Category)
	assert.Equal(t, "tc1", msgs[1].ToolCalls[0].ToolUseID)
}

func TestZencoderProviderParsesToolResults(t *testing.T) {
	header := `{"id":"tr-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Read it."}]}`
	assistant := `{"role":"assistant","content":[` +
		`{"type":"tool-call","toolCallId":"tc1","toolName":"Read","input":{"file_path":"main.go"}}` +
		`]}`
	tool := `{"role":"tool","content":[` +
		`{"type":"tool-result","toolCallId":"tc1","toolName":"Read","content":[{"type":"text","text":"package main"}],"isError":false}` +
		`]}`

	content := strings.Join([]string{
		header, user, assistant, tool,
	}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertMessageCount(t, sess.MessageCount, 3)

	// Tool result is emitted as RoleUser message.
	assert.Equal(t, RoleUser, msgs[2].Role)
	require.Equal(t, 1, len(msgs[2].ToolResults))
	assert.Equal(t, "tc1", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(t, len("package main"),
		msgs[2].ToolResults[0].ContentLength)
	// ContentRaw must be populated so pairToolResults can
	// decode tool output for display.
	assert.NotEmpty(t, msgs[2].ToolResults[0].ContentRaw,
		"ContentRaw should contain raw JSON of tool result content")
	assert.Contains(t, msgs[2].ToolResults[0].ContentRaw,
		"package main")
}

func TestZencoderProviderParsesUserInputTagFiltering(t *testing.T) {
	header := `{"id":"tag-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[` +
		`{"type":"text","text":"system instructions","tag":"instructions"},` +
		`{"type":"text","text":"actual user input","tag":"user-input"},` +
		`{"type":"text","text":"todo reminder","tag":"todo-reminder"}` +
		`]}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"Got it."}]}`

	content := strings.Join([]string{
		header, user, assistant,
	}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// User message has only "user-input" tagged content.
	assert.Equal(t, "actual user input", msgs[0].Content)
	assert.False(t, msgs[0].IsSystem)
	assert.NotContains(t, msgs[0].Content, "system instructions")
	assert.NotContains(t, msgs[0].Content, "todo reminder")

	// System-tagged content stored as separate system message.
	assert.True(t, msgs[1].IsSystem)
	assert.Equal(t, RoleUser, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "system instructions")
	assert.Contains(t, msgs[1].Content, "todo reminder")

	// Assistant message follows.
	assert.Equal(t, RoleAssistant, msgs[2].Role)
	assert.False(t, msgs[2].IsSystem)

	// UserMessageCount should exclude system messages.
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "actual user input", sess.FirstMessage)
}

func TestZencoderProviderParsesDirectContinuation(t *testing.T) {
	header := `{"id":"child-123","parentId":"parent-456","creationReason":"directContinuation","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Continue."}]}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"Continuing."}]}`

	content := strings.Join([]string{
		header, user, assistant,
	}, "\n")

	sess, _, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, "zencoder:parent-456", sess.ParentSessionID)
	assert.Equal(t, RelContinuation, sess.RelationshipType)
}

func TestZencoderProviderParsesSummarizedContinuation(t *testing.T) {
	header := `{"id":"child-789","parentId":"parent-012","creationReason":"summarizedContinuation","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Continue."}]}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"OK."}]}`

	content := strings.Join([]string{
		header, user, assistant,
	}, "\n")

	sess, _, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, "zencoder:parent-012", sess.ParentSessionID)
	assert.Equal(t, RelContinuation, sess.RelationshipType)
}

func TestZencoderProviderParsesProjectExtraction(t *testing.T) {
	header := `{"id":"proj-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	system := `{"role":"system","content":"You are helpful.\n\nWorking directory: /home/user/workspace/coolproject\n"}`
	user := `{"role":"user","content":[{"type":"text","text":"hello"}]}`

	content := strings.Join([]string{
		header, system, user,
	}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, "coolproject", sess.Project)
	// System message stored + user message = 2
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.True(t, msgs[0].IsSystem)
	assert.False(t, msgs[1].IsSystem)
}

func TestZencoderProviderParsesEmptySession(t *testing.T) {
	// Header only, no messages.
	header := `{"id":"empty-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`

	sess, msgs, err := runZencoderParserTest(t, header)
	require.NoError(t, err)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestZencoderProviderParsesPermissionSkippedFinishStored(t *testing.T) {
	header := `{"id":"skip-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Do it."}]}`
	permission := `{"role":"permission","data":{"allowed":true}}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"Done."}]}`
	finish := `{"role":"finish","reason":"endTurn"}`

	content := strings.Join([]string{
		header, user, permission, assistant, finish,
	}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// permission is skipped; finish is stored as system.
	// user + assistant + finish = 3
	assertMessageCount(t, sess.MessageCount, 3)
	require.Equal(t, 3, len(msgs))
	assert.False(t, msgs[0].IsSystem) // user
	assert.False(t, msgs[1].IsSystem) // assistant
	assert.True(t, msgs[2].IsSystem)  // finish
	assert.Equal(t, "[Turn finished: endTurn]", msgs[2].Content)
	assert.Equal(t, 1, sess.UserMessageCount)
}

func TestZencoderProviderParsesFirstMessageTruncation(t *testing.T) {
	header := `{"id":"trunc-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	longText := strings.Repeat("a", 400)
	user := `{"role":"user","content":[{"type":"text","text":"` + longText + `"}]}`

	content := strings.Join([]string{header, user}, "\n")

	sess, _, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)
	// truncate clips at 300 chars + 3 ellipsis chars = 303.
	assert.Equal(t, 303, len(sess.FirstMessage))
}

func TestZencoderProviderParsesMissingFile(t *testing.T) {
	sess, msgs, err := parseZencoderTestSession(t, "/nonexistent/test.jsonl", "local")
	require.NoError(t, err)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestZencoderProviderParsesFallbackSessionID(t *testing.T) {
	// Header with no id field -> falls back to filename.
	header := `{"createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"hello"}]}`

	content := strings.Join([]string{header, user}, "\n")

	sess, _, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)
	// Falls back to filename-derived ID.
	assert.Equal(t, "zencoder:test-uuid", sess.ID)
}

func TestZencoderProviderDiscoversSessions(t *testing.T) {
	dir := t.TempDir()

	// Create some session files.
	for _, name := range []string{
		"abc-123.jsonl",
		"def-456.jsonl",
		"not-jsonl.txt",
	} {
		f, err := os.Create(filepath.Join(dir, name))
		require.NoError(t, err)
		f.Close()
	}

	// Create a subdirectory (should be skipped).
	require.NoError(t, os.Mkdir(
		filepath.Join(dir, "subdir"), 0o755,
	))

	provider, ok := NewProvider(AgentZencoder, ProviderConfig{
		Roots:   []string{dir},
		Machine: "local",
	})
	require.True(t, ok)
	files, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, len(files))
	for _, f := range files {
		assert.Equal(t, AgentZencoder, f.Provider)
		assert.True(t, strings.HasSuffix(f.DisplayPath, ".jsonl"))
	}
}

func TestZencoderProviderDiscoversEmptyDir(t *testing.T) {
	provider, ok := NewProvider(AgentZencoder, ProviderConfig{
		Roots:   []string{""},
		Machine: "local",
	})
	require.True(t, ok)
	files, err := provider.Discover(context.Background())
	require.NoError(t, err)
	assert.Empty(t, files)
}

func TestZencoderProviderFindsSourceFile(t *testing.T) {
	dir := t.TempDir()
	name := "abc-def-123.jsonl"
	f, err := os.Create(filepath.Join(dir, name))
	require.NoError(t, err)
	f.Close()

	provider, ok := NewProvider(AgentZencoder, ProviderConfig{
		Roots:   []string{dir},
		Machine: "local",
	})
	require.True(t, ok)
	found, ok, err := provider.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: "abc-def-123"},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, filepath.Join(dir, name), found.DisplayPath)

	// Non-existent ID.
	_, ok, err = provider.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: "nonexistent"},
	)
	require.NoError(t, err)
	assert.False(t, ok)

	// Empty dir.
	emptyProvider, ok := NewProvider(AgentZencoder, ProviderConfig{
		Roots:   []string{""},
		Machine: "local",
	})
	require.True(t, ok)
	_, ok, err = emptyProvider.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: "abc-def-123"},
	)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestZencoderProviderParsesUserContentWithoutTag(t *testing.T) {
	header := `{"id":"notag-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"no tag input"}]}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"OK."}]}`

	content := strings.Join([]string{
		header, user, assistant,
	}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Content without tag should be included.
	assert.Equal(t, "no tag input", msgs[0].Content)
}

func TestZencoderProviderParsesNewChatNoRelationship(t *testing.T) {
	header := `{"id":"new-123","parentId":"some-parent","creationReason":"newChat","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"hello"}]}`

	content := strings.Join([]string{header, user}, "\n")

	sess, _, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// newChat even with parentId -> no relationship.
	assert.Empty(t, sess.ParentSessionID)
	assert.Equal(t, RelNone, sess.RelationshipType)
}

func TestZencoderProviderParsesSubagentSessionID(t *testing.T) {
	header := `{"id":"parent-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Use subagent."}]}`
	assistant := `{"role":"assistant","content":[` +
		`{"type":"tool-call","toolCallId":"tc_sub1","toolName":"mcp__zen_subagents__spawn_subagent","input":{"prompt":"do stuff","agent":"research"}}` +
		`]}`
	tool := `{"role":"tool","content":[` +
		`{"type":"tool-result","toolCallId":"tc_sub1","toolName":"mcp__zen_subagents__spawn_subagent","content":[{"type":"text","text":"Subagent completed.\n<session-id>child-abc-123</session-id>"}],"isError":false}` +
		`]}`

	content := strings.Join([]string{
		header, user, assistant, tool,
	}, "\n")

	_, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)

	// The assistant message should have the tool call with
	// SubagentSessionID set from the tool-result's <session-id>.
	require.Equal(t, 3, len(msgs))
	require.Equal(t, 1, len(msgs[1].ToolCalls))
	assert.Equal(t,
		"zencoder:child-abc-123",
		msgs[1].ToolCalls[0].SubagentSessionID,
	)
}

func TestZencoderProviderParsesSubagentMultiple(t *testing.T) {
	header := `{"id":"parent-456","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Use subagents."}]}`
	assistant := `{"role":"assistant","content":[` +
		`{"type":"tool-call","toolCallId":"tc_a","toolName":"Zencoder_subagent__ZencoderSubagent","input":{"prompt":"task A","agent":"plan"}},` +
		`{"type":"tool-call","toolCallId":"tc_b","toolName":"mcp__zen_subagents__spawn_subagent","input":{"prompt":"task B","agent":"research"}}` +
		`]}`
	toolA := `{"role":"tool","content":[` +
		`{"type":"tool-result","toolCallId":"tc_a","content":[{"type":"text","text":"Done A.\n<session-id>child-aaa</session-id>"}],"isError":false}` +
		`]}`
	toolB := `{"role":"tool","content":[` +
		`{"type":"tool-result","toolCallId":"tc_b","content":[{"type":"text","text":"Done B.\n<session-id>child-bbb</session-id>"}],"isError":false}` +
		`]}`

	content := strings.Join([]string{
		header, user, assistant, toolA, toolB,
	}, "\n")

	_, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)

	require.Equal(t, 2, len(msgs[1].ToolCalls))
	assert.Equal(t,
		"zencoder:child-aaa",
		msgs[1].ToolCalls[0].SubagentSessionID,
	)
	assert.Equal(t,
		"zencoder:child-bbb",
		msgs[1].ToolCalls[1].SubagentSessionID,
	)
}

func TestZencoderProviderParsesNoSessionIDTag(t *testing.T) {
	header := `{"id":"parent-789","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Read file."}]}`
	assistant := `{"role":"assistant","content":[` +
		`{"type":"tool-call","toolCallId":"tc_read","toolName":"Read","input":{"file_path":"main.go"}}` +
		`]}`
	tool := `{"role":"tool","content":[` +
		`{"type":"tool-result","toolCallId":"tc_read","content":[{"type":"text","text":"package main"}],"isError":false}` +
		`]}`

	content := strings.Join([]string{
		header, user, assistant, tool,
	}, "\n")

	_, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)

	// Non-subagent tool call should have empty SubagentSessionID.
	require.Equal(t, 1, len(msgs[1].ToolCalls))
	assert.Empty(t, msgs[1].ToolCalls[0].SubagentSessionID)
}

func TestZencoderProviderParsesSkillBlocks(t *testing.T) {
	header := `{"id":"skill-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[` +
		`{"type":"text","text":"Do the thing.","tag":"user-input"},` +
		`{"type":"text","text":"system instructions here","tag":"instructions"},` +
		`{"type":"skill","name":"init","path":"/path/to/SKILL.md","content":"skill body content","disableModelInvocation":true}` +
		`]}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"OK."}]}`

	content := strings.Join([]string{
		header, user, assistant,
	}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// msg[0]: user message (only user-input text)
	assert.Equal(t, "Do the thing.", msgs[0].Content)
	assert.False(t, msgs[0].IsSystem)

	// msg[1]: system message (instructions + skill)
	assert.True(t, msgs[1].IsSystem)
	assert.Equal(t, RoleUser, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "system instructions here")
	assert.Contains(t, msgs[1].Content, "[Skill: init]")
	assert.Contains(t, msgs[1].Content, "skill body content")
	assert.Contains(t, msgs[1].Content, "[/Skill]")

	// msg[2]: assistant
	assert.Equal(t, RoleAssistant, msgs[2].Role)

	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "Do the thing.", sess.FirstMessage)
}

func TestZencoderProviderParsesToolResultSystemTags(t *testing.T) {
	header := `{"id":"trsys-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Run it."}]}`
	assistant := `{"role":"assistant","content":[` +
		`{"type":"tool-call","toolCallId":"tc1","toolName":"Bash","input":{"command":"ls"}}` +
		`]}`
	tool := `{"role":"tool","content":[` +
		`{"type":"tool-result","toolCallId":"tc1","toolName":"Bash","content":[` +
		`{"type":"shell-result","text":"file1.go\nfile2.go"},` +
		`{"type":"text","tag":"todo-reminder","text":"<system-reminder>Remember your tasks</system-reminder>"},` +
		`{"type":"text","tag":"system-reminder","text":"<system-reminder>Extra context</system-reminder>"}` +
		`],"isError":false}` +
		`]}`

	content := strings.Join([]string{
		header, user, assistant, tool,
	}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// msg[0]: user, msg[1]: assistant, msg[2]: tool result,
	// msg[3]: system message from tool-result tags
	require.Equal(t, 4, len(msgs))

	// Tool result message is unaffected.
	assert.Equal(t, RoleUser, msgs[2].Role)
	assert.False(t, msgs[2].IsSystem)
	require.Equal(t, 1, len(msgs[2].ToolResults))
	assert.Equal(t, "tc1", msgs[2].ToolResults[0].ToolUseID)

	// System message from tool-result tags.
	assert.True(t, msgs[3].IsSystem)
	assert.Equal(t, RoleUser, msgs[3].Role)
	assert.Contains(t, msgs[3].Content, "Remember your tasks")
	assert.Contains(t, msgs[3].Content, "Extra context")
}

func TestZencoderProviderParsesToolResultTaggedBlocksFilteredFromContentRaw(t *testing.T) {
	// Verify that tagged text blocks in tool-result content are
	// stripped from ContentRaw (to avoid double-rendering) and
	// emitted as a separate system message instead.
	tests := []struct {
		name            string
		toolContent     string
		wantInRaw       []string // substrings that MUST be in ContentRaw
		wantNotInRaw    []string // substrings that must NOT be in ContentRaw
		wantSystemParts []string // substrings in the system message
		wantContentLen  int      // expected ContentLength of tool result
		wantSystemMsg   bool     // expect a separate system message
		wantMsgCount    int      // total messages (user + assistant + tool + maybe system)
	}{
		{
			name: "tagged blocks stripped, regular output kept",
			toolContent: `[` +
				`{"type":"shell-result","text":"file1.go\nfile2.go"},` +
				`{"type":"text","tag":"system-reminder","text":"Remember your tasks"},` +
				`{"type":"text","tag":"todo-reminder","text":"Check your TODOs"}` +
				`]`,
			wantInRaw:       []string{"file1.go", "shell-result"},
			wantNotInRaw:    []string{"Remember your tasks", "Check your TODOs", "system-reminder", "todo-reminder"},
			wantSystemParts: []string{"Remember your tasks", "Check your TODOs"},
			wantContentLen:  len("file1.go\nfile2.go"),
			wantSystemMsg:   true,
			wantMsgCount:    4, // user + assistant + tool + system
		},
		{
			name: "no tagged blocks, ContentRaw unchanged",
			toolContent: `[` +
				`{"type":"text","text":"plain output"}` +
				`]`,
			wantInRaw:      []string{"plain output"},
			wantNotInRaw:   nil,
			wantContentLen: len("plain output"),
			wantSystemMsg:  false,
			wantMsgCount:   3, // user + assistant + tool
		},
		{
			name: "all blocks tagged, ContentRaw is empty array",
			toolContent: `[` +
				`{"type":"text","tag":"instructions","text":"sys only"}` +
				`]`,
			wantInRaw:       nil,
			wantNotInRaw:    []string{"sys only", "instructions"},
			wantSystemParts: []string{"sys only"},
			wantContentLen:  0,
			wantSystemMsg:   true,
			wantMsgCount:    4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := `{"id":"filt-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
			user := `{"role":"user","content":[{"type":"text","text":"Run it."}]}`
			assistant := `{"role":"assistant","content":[` +
				`{"type":"tool-call","toolCallId":"tc1","toolName":"Bash","input":{"command":"ls"}}` +
				`]}`
			tool := `{"role":"tool","content":[` +
				`{"type":"tool-result","toolCallId":"tc1","toolName":"Bash","content":` +
				tt.toolContent +
				`,"isError":false}` +
				`]}`

			content := strings.Join([]string{
				header, user, assistant, tool,
			}, "\n")

			sess, msgs, err := runZencoderParserTest(t, content)
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.Equal(t, tt.wantMsgCount, len(msgs))

			// Tool result message is always at index 2.
			toolMsg := msgs[2]
			require.Equal(t, 1, len(toolMsg.ToolResults))
			tr := toolMsg.ToolResults[0]

			// Verify ContentLength matches filtered content.
			assert.Equal(t, tt.wantContentLen, tr.ContentLength,
				"ContentLength should reflect filtered content")

			// Verify expected substrings are present in ContentRaw.
			for _, s := range tt.wantInRaw {
				assert.Contains(t, tr.ContentRaw, s,
					"ContentRaw should contain %q", s)
			}

			// Verify tagged text is NOT in ContentRaw.
			for _, s := range tt.wantNotInRaw {
				assert.NotContains(t, tr.ContentRaw, s,
					"ContentRaw should not contain tagged text %q", s)
			}

			// Verify DecodeContent on the filtered ContentRaw
			// does not include tagged text.
			decoded := DecodeContent(tr.ContentRaw)
			for _, s := range tt.wantNotInRaw {
				assert.NotContains(t, decoded, s,
					"DecodeContent should not return tagged text %q", s)
			}

			if tt.wantSystemMsg {
				// System message is the last message.
				sysMsg := msgs[len(msgs)-1]
				assert.True(t, sysMsg.IsSystem,
					"last message should be a system message")
				assert.Equal(t, RoleUser, sysMsg.Role)
				for _, s := range tt.wantSystemParts {
					assert.Contains(t, sysMsg.Content, s,
						"system message should contain %q", s)
				}
			}
		})
	}
}

func TestZencoderProviderParsesSystemOnlySession(t *testing.T) {
	// A session with only a header and a system message (e.g.
	// environment banner) should be filtered out as empty.
	header := `{"id":"sysonly-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	system := `{"role":"system","content":"You are an AI assistant.\n\nWorking directory: /home/user/proj"}`

	content := strings.Join([]string{header, system}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	assert.Nil(t, sess, "system-only session should be nil")
	assert.Nil(t, msgs, "system-only session should produce no messages")
}

func TestZencoderProviderParsesSystemAndFinishOnlySession(t *testing.T) {
	// A session with system + finish but no real user/assistant
	// messages should also be filtered out.
	header := `{"id":"sysfin-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	system := `{"role":"system","content":"You are an AI assistant."}`
	finish := `{"role":"finish","reason":"endTurn"}`

	content := strings.Join([]string{header, system, finish}, "\n")

	sess, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	assert.Nil(t, sess, "system+finish-only session should be nil")
	assert.Nil(t, msgs)
}

func TestZencoderProviderParsesTimestampBoundsFromMessages(t *testing.T) {
	// When header timestamps are missing, session bounds should
	// be derived from per-message timestamps.
	header := `{"id":"bounds-123"}`
	user := `{"role":"user","content":[{"type":"text","text":"Hello."}],"createdAt":"2024-01-01T10:00:00Z"}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"Hi."}],"createdAt":"2024-01-01T10:05:00Z"}`
	finish := `{"role":"finish","reason":"endTurn","createdAt":"2024-01-01T10:05:01Z"}`

	content := strings.Join([]string{
		header, user, assistant, finish,
	}, "\n")

	sess, _, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	wantStart := mustParseTime(t, "2024-01-01T10:00:00Z")
	wantEnd := mustParseTime(t, "2024-01-01T10:05:01Z")
	assertTimestamp(t, sess.StartedAt, wantStart)
	assertTimestamp(t, sess.EndedAt, wantEnd)
}

func TestZencoderProviderParsesTimestampBoundsStaleHeader(t *testing.T) {
	// When header has timestamps but messages have more
	// recent ones, endedAt should be updated.
	header := `{"id":"stale-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Hello."}],"createdAt":"2024-01-01T00:02:00Z"}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"Hi."}],"createdAt":"2024-01-01T00:10:00Z"}`

	content := strings.Join([]string{
		header, user, assistant,
	}, "\n")

	sess, _, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.NotNil(t, sess)

	// startedAt stays at header value since it's earlier.
	wantStart := mustParseTime(t, "2024-01-01T00:00:00Z")
	assertTimestamp(t, sess.StartedAt, wantStart)

	// endedAt should be updated to the latest message time.
	wantEnd := mustParseTime(t, "2024-01-01T00:10:00Z")
	assertTimestamp(t, sess.EndedAt, wantEnd)
}

func TestZencoderProviderParsesMessageTimestamps(t *testing.T) {
	header := `{"id":"ts-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:05:00Z"}`
	system := `{"role":"system","content":"You are an AI.\n\nWorking directory: /home/user/proj","createdAt":"2024-01-01T00:00:01Z"}`
	user := `{"role":"user","content":[{"type":"text","text":"Hello."}],"createdAt":"2024-01-01T00:00:02Z"}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"Hi."}],"createdAt":"2024-01-01T00:00:03Z"}`
	tool := `{"role":"tool","content":[` +
		`{"type":"tool-result","toolCallId":"tc1","content":[{"type":"text","text":"ok"}],"isError":false}` +
		`],"createdAt":"2024-01-01T00:00:04Z"}`
	finish := `{"role":"finish","reason":"endTurn","createdAt":"2024-01-01T00:00:05Z"}`

	content := strings.Join([]string{
		header, system, user, assistant, tool, finish,
	}, "\n")

	_, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.Equal(t, 5, len(msgs))

	// System message.
	wantSys := mustParseTime(t, "2024-01-01T00:00:01Z")
	assert.Equal(t, wantSys, msgs[0].Timestamp)

	// User message.
	wantUser := mustParseTime(t, "2024-01-01T00:00:02Z")
	assert.Equal(t, wantUser, msgs[1].Timestamp)

	// Assistant message.
	wantAssistant := mustParseTime(t, "2024-01-01T00:00:03Z")
	assert.Equal(t, wantAssistant, msgs[2].Timestamp)

	// Tool result message.
	wantTool := mustParseTime(t, "2024-01-01T00:00:04Z")
	assert.Equal(t, wantTool, msgs[3].Timestamp)

	// Finish message.
	wantFinish := mustParseTime(t, "2024-01-01T00:00:05Z")
	assert.Equal(t, wantFinish, msgs[4].Timestamp)
}

func TestZencoderProviderParsesMessageTimestamps_Missing(t *testing.T) {
	header := `{"id":"ts-miss-123","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:01:00Z"}`
	// Lines without createdAt field.
	user := `{"role":"user","content":[{"type":"text","text":"No timestamp."}]}`
	assistant := `{"role":"assistant","content":[{"type":"text","text":"Also none."}]}`

	content := strings.Join([]string{
		header, user, assistant,
	}, "\n")

	_, msgs, err := runZencoderParserTest(t, content)
	require.NoError(t, err)
	require.Equal(t, 2, len(msgs))

	// Both messages should have zero time when createdAt is missing.
	assert.True(t, msgs[0].Timestamp.IsZero())
	assert.True(t, msgs[1].Timestamp.IsZero())
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts := parseTimestamp(s)
	if ts.IsZero() {
		t.Fatalf("failed to parse timestamp %q", s)
	}
	return ts
}
