package parser

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// codebuffWriteFile writes body to path, creating parent dirs.
func codebuffWriteFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

// codebuffTestSession creates a minimal codebuff session directory with
// chat-messages.json, run-state.json, and chat-meta.json. Returns the
// session directory path.
func codebuffTestSession(
	t *testing.T,
	chatMessages string,
	runState string,
	chatMeta string,
) string {
	t.Helper()
	dir := t.TempDir()
	codebuffWriteFile(t, filepath.Join(dir, "chat-messages.json"), chatMessages)
	if runState != "" {
		codebuffWriteFile(t, filepath.Join(dir, "run-state.json"), runState)
	}
	if chatMeta != "" {
		codebuffWriteFile(t, filepath.Join(dir, "chat-meta.json"), chatMeta)
	}
	return dir
}

func TestParseCodebuffSession_BasicUserAndAIMessages(t *testing.T) {
	chatMessages := `[
		{
			"id": "user-1",
			"variant": "user",
			"content": "Fix the login bug",
			"timestamp": "2026-07-15T15:04:00Z"
		},
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "2026-07-15T15:05:00Z",
			"blocks": [
				{
					"type": "text",
					"textType": "text",
					"content": "I'll fix the login bug by updating the auth handler."
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek",
				"contextTokenCount": 50000
			},
			"fileContext": {
				"cwd": "/Users/dev/myproject"
			}
		}
	}`
	chatMeta := `{"messageCount": 2, "firstPrompt": "Fix the login bug", "messagesSize": 1024}`

	dir := codebuffTestSession(t, chatMessages, runState, chatMeta)
	sess, msgs, _, err := parseCodebuffSession(dir, "myproject", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, AgentCodebuff, sess.Agent)
	assert.Equal(t, "Codebuff", sess.AgentLabel)
	assert.Contains(t, sess.ID, "codebuff:")
	assert.Equal(t, "myproject", sess.Project)
	assert.Equal(t, "/Users/dev/myproject", sess.Cwd)
	assert.Equal(t, "codebuff-chat-v1", sess.SourceVersion)
	assert.Equal(t, "Fix the login bug", sess.FirstMessage)
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	// PeakContextTokens is not set because contextTokenCount from
	// run-state.json is the final per-step count, not the peak.

	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Fix the login bug", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "I'll fix the login bug")
}

func TestParseCodebuffSession_FreebuffClassification(t *testing.T) {
	chatMessages := `[
		{
			"id": "user-1",
			"variant": "user",
			"content": "Hello",
			"timestamp": "10:00 AM"
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-free-minimax-m3"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	sess, _, _, err := parseCodebuffSession(dir, "testproject", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Freebuff sessions use AgentFreebuff for distinct filtering, while
	// lifecycle operations are handled via the freebuff: prefix alias
	// in AgentByPrefix.
	assert.Equal(t, AgentFreebuff, sess.Agent)
	assert.Equal(t, "Freebuff", sess.AgentLabel)
}

func TestParseCodebuffSession_Timestamps(t *testing.T) {
	chatMessages := `[
		{
			"id": "user-1",
			"variant": "user",
			"content": "first",
			"timestamp": "2026-07-15T15:04:00Z"
		},
		{
			"id": "user-2",
			"variant": "user",
			"content": "second",
			"timestamp": "2026-07-15T15:10:00Z"
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	sess, _, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.False(t, sess.StartedAt.IsZero())
	assert.False(t, sess.EndedAt.IsZero())
	assert.True(t, sess.EndedAt.After(sess.StartedAt) || sess.EndedAt.Equal(sess.StartedAt))
}

func TestParseCodebuffSession_ToolCalls(t *testing.T) {
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "tool",
					"toolName": "read_files",
					"toolCallId": "tc-1",
					"input": {"paths": ["src/main.go"]},
					"output": "package main"
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	// AI message with tool call + tool result = 2 messages.
	require.GreaterOrEqual(t, len(msgs), 1)

	var toolCallMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].HasToolUse {
			toolCallMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolCallMsg, "expected a message with tool use")
	assert.Equal(t, RoleAssistant, toolCallMsg.Role)
	require.Len(t, toolCallMsg.ToolCalls, 1)
	assert.Equal(t, "read_files", toolCallMsg.ToolCalls[0].ToolName)
	assert.Equal(t, "Read", toolCallMsg.ToolCalls[0].Category)
	assert.Equal(t, "tc-1", toolCallMsg.ToolCalls[0].ToolUseID)
	assert.Contains(t, toolCallMsg.ToolCalls[0].InputJSON, "src/main.go")
}

func TestParseCodebuffSession_SubagentToolCall(t *testing.T) {
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "agent",
					"agentId": "agent-1",
					"agentName": "basher",
					"agentType": "basher",
					"status": "complete",
					"initialPrompt": "run tests",
					"content": "All tests passed."
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	var toolCallMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].HasToolUse {
			toolCallMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolCallMsg, "expected a message with tool use")
	require.Len(t, toolCallMsg.ToolCalls, 1)

	tc := toolCallMsg.ToolCalls[0]
	assert.Equal(t, "Task", tc.Category, "subagent calls should use Task category")
	assert.Equal(t, "basher", tc.ToolName)
	assert.Equal(t, "agent-1", tc.ToolUseID)
	assert.Contains(t, tc.InputJSON, "basher")
	assert.Contains(t, tc.InputJSON, "run tests")
	// The agent's lifecycle status must be carried in the tool-call input
	// now that agent output is emitted as a linked ParsedToolResult; it
	// used to render in the assistant text for the block. Assert on the
	// decoded value, not a raw substring, so formatting changes to
	// InputJSON cannot silently drop the field.
	var input map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.InputJSON), &input))
	status, ok := input["status"]
	require.True(t, ok,
		"agent tool-call InputJSON must include the status field")
	assert.Equal(t, "complete", status,
		"agent status must round-trip from the block's status field")
	// The initialPrompt alone gives the subagent a child session to link.
	assert.Equal(t, "codebuff:p:"+filepath.Base(dir)+"__subagent__agent-1",
		tc.SubagentSessionID)
}

// TestParseCodebuffSession_SubagentToolCallDefaultStatus pins the default
// lifecycle status for agent blocks that omit the status field. The parser
// folds the status into the tool-call InputJSON; when the block carries no
// status, it must default to "spawned" so the field stays present for
// consumers that key off it.
func TestParseCodebuffSession_SubagentToolCallDefaultStatus(t *testing.T) {
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "agent",
					"agentId": "agent-1",
					"agentName": "basher",
					"agentType": "basher",
					"initialPrompt": "run tests",
					"content": "All tests passed."
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	var toolCallMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].HasToolUse {
			toolCallMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolCallMsg, "expected a message with tool use")
	require.Len(t, toolCallMsg.ToolCalls, 1)

	tc := toolCallMsg.ToolCalls[0]
	var input map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.InputJSON), &input))
	status, ok := input["status"]
	require.True(t, ok,
		"agent tool-call InputJSON must always include the status field")
	assert.Equal(t, "spawned", status,
		"a missing block status must default to spawned")
}

func TestParseCodebuffSession_ThinkingBlocks(t *testing.T) {
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "text",
					"textType": "reasoning",
					"content": "Let me think about this approach."
				},
				{
					"type": "text",
					"textType": "text",
					"content": "Here is my answer."
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	var thinkingMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].HasThinking {
			thinkingMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, thinkingMsg, "expected a thinking message")
	assert.Equal(t, RoleAssistant, thinkingMsg.Role)
	assert.Contains(t, thinkingMsg.Content, "[Thinking]")
	assert.Contains(t, thinkingMsg.Content, "Let me think about this approach.")
	assert.Contains(t, thinkingMsg.ThinkingText, "Let me think about this approach.")
}

func TestParseCodebuffSession_ModeDivider(t *testing.T) {
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "mode-divider",
					"mode": "LITE"
				},
				{
					"type": "text",
					"textType": "text",
					"content": "Working in LITE mode."
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	var sysMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].IsSystem {
			sysMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, sysMsg, "expected a system message from mode divider")
	assert.Equal(t, RoleSystem, sysMsg.Role)
	assert.Contains(t, sysMsg.Content, "[Mode: LITE]")
}

func TestParseCodebuffSession_EmptyMessages(t *testing.T) {
	chatMessages := `[]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	sess, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Empty(t, msgs)
	assert.Equal(t, 0, sess.MessageCount)
}

func TestParseCodebuffSession_MissingRunState(t *testing.T) {
	chatMessages := `[
		{
			"id": "user-1",
			"variant": "user",
			"content": "Hello",
			"timestamp": "03:04 PM"
		}
	]`

	dir := codebuffTestSession(t, chatMessages, "", "")
	sess, _, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, AgentCodebuff, sess.Agent)
	assert.Empty(t, sess.Cwd)
	assert.False(t, sess.HasPeakContextTokens)
}

func TestParseCodebuffSessionFromChatMeta(t *testing.T) {
	chatMessages := `[]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`
	chatMeta := `{
		"messageCount": 5,
		"firstPrompt": "Fix the login bug",
		"messagesSize": 2048
	}`

	dir := codebuffTestSession(t, chatMessages, runState, chatMeta)
	sess, _, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// When transcript is empty, chat-meta counts are used as fallback.
	assert.Equal(t, 5, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "Fix the login bug", sess.FirstMessage)
	// CountsAuthoritative must be set when chat-meta is the only count
	// source. Without this, the sync engine's
	// applySessionTokenTotalsFromMessages pass recomputes counts from
	// the empty parsed-message slice and overwrites the meta totals
	// with zero, hiding the session from any UI that filters on
	// nonzero counts.
	assert.True(t, sess.CountsAuthoritative,
		"counts from the chat-meta fallback must be authoritative "+
			"so sync does not zero them out")
}

// TestParseCodebuffSessionFromTranscriptLeavesCountsNonAuthoritative
// confirms that CountsAuthoritative stays false when the transcript
// itself supplies the counts. Marking it true in that case would
// suppress the sync engine's reconciling pass for sessions with real
// message rows, hiding any future transcript-driven count drift.
func TestParseCodebuffSessionFromTranscriptLeavesCountsNonAuthoritative(
	t *testing.T,
) {
	chatMessages := `[
		{"id":"user-1","variant":"user","content":"hi","timestamp":"03:04 PM"}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-deepseek"}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, `{}`)
	sess, _, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.False(t, sess.CountsAuthoritative,
		"counts derived from a non-empty transcript must remain "+
			"non-authoritative so the sync engine can reconcile "+
			"them from message rows")
}

// TestParseCodebuffSessionEmptyChatMetaLeavesCountsNonAuthoritative
// covers the edge case where the transcript is empty (chat-messages.json
// is `[]`) and chat-meta.json has no messageCount field, so the meta
// fallback has nothing authoritative to offer. CountsAuthoritative
// must stay false: the session is just unparseable for messaging, and
// the sync engine should not be told to skip its count-recompute pass
// for sessions like this (otherwise we'd silently drop zero-count
// rows that other fields still rely on).
func TestParseCodebuffSessionEmptyChatMetaLeavesCountsNonAuthoritative(
	t *testing.T,
) {
	chatMessages := `[]`
	runState := `{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-deepseek"}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, `{}`)
	sess, _, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.Equal(t, 0, sess.MessageCount)
	assert.False(t, sess.CountsAuthoritative,
		"empty transcript and empty meta must keep counts "+
			"non-authoritative so the sync engine sees a real "+
			"zero rather than silently skipping its recompute")
}

// TestParseCodebuffSessionGitBranch covers branch capture from run-state.json;
// gitChanges is absent when git is unavailable.
func TestParseCodebuffSessionGitBranch(t *testing.T) {
	chatMessages := `[
		{"id":"user-1","variant":"user","content":"hi","timestamp":"03:04 PM"}
	]`

	t.Run("branch present", func(t *testing.T) {
		runState := `{
			"sessionState": {
				"mainAgentState": {"agentType": "base2-deepseek"},
				"fileContext": {
					"gitChanges": {"branch": "feat/agent-view"}
				}
			}
		}`
		dir := codebuffTestSession(t, chatMessages, runState, "")
		sess, _, _, err := parseCodebuffSession(dir, "proj", "local")
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, "feat/agent-view", sess.GitBranch)
	})

	t.Run("gitChanges absent leaves branch empty", func(t *testing.T) {
		runState := `{
			"sessionState": {
				"mainAgentState": {"agentType": "base2-deepseek"}
			}
		}`
		dir := codebuffTestSession(t, chatMessages, runState, "")
		sess, _, _, err := parseCodebuffSession(dir, "proj", "local")
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Empty(t, sess.GitBranch)
	})
}

// TestParseCodebuffSessionTerminationStatus covers the ending
// classification from the last assistant turn.
func TestParseCodebuffSessionTerminationStatus(t *testing.T) {
	runState := `{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-deepseek"}
		}
	}`

	t.Run("unresolved tool call is pending", func(t *testing.T) {
		chatMessages := `[
			{"id":"user-1","variant":"user","content":"run the tests","timestamp":"03:04 PM"},
			{
				"id":"ai-1","variant":"ai","content":"","timestamp":"03:05 PM",
				"blocks": [
					{
						"type": "tool",
						"toolCallId": "call-1",
						"toolName": "run_terminal_command",
						"input": {"command": "go test ./..."}
					}
				]
			}
		]`
		dir := codebuffTestSession(t, chatMessages, runState, "")
		sess, _, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, TerminationToolCallPending, sess.TerminationStatus)
	})

	t.Run("resolved ending is clean", func(t *testing.T) {
		chatMessages := `[
			{"id":"user-1","variant":"user","content":"hi","timestamp":"03:04 PM"},
			{
				"id":"ai-1","variant":"ai","content":"All done.","timestamp":"03:05 PM"
			}
		]`
		dir := codebuffTestSession(t, chatMessages, runState, "")
		sess, _, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Equal(t, TerminationClean, sess.TerminationStatus)
	})

	t.Run("empty transcript is unknown", func(t *testing.T) {
		dir := codebuffTestSession(t, `[]`, runState, "")
		sess, _, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.NotNil(t, sess)
		assert.Empty(t, sess.TerminationStatus,
			"no messages means no classification")
	})
}

func TestParseCodebuffSession_ProjectFromCwd(t *testing.T) {
	chatMessages := `[
		{
			"id": "user-1",
			"variant": "user",
			"content": "Hello",
			"timestamp": "03:04 PM"
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			},
			"fileContext": {
				"cwd": "/Users/dev/projects/myproject"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	sess, _, _, err := parseCodebuffSession(dir, "fallback", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Cwd-based project extraction takes precedence over hint.
	assert.NotEmpty(t, sess.Project)
}

func TestParseCodebuffSession_FileInfo(t *testing.T) {
	chatMessages := `[
		{
			"id": "user-1",
			"variant": "user",
			"content": "Hello",
			"timestamp": "03:04 PM"
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	sess, _, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.NotEmpty(t, sess.File.Path)
	assert.Positive(t, sess.File.Size)
	assert.NotZero(t, sess.File.Mtime)
}

func TestParseCodebuffSession_JoinedTextContent(t *testing.T) {
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "text",
					"textType": "text",
					"content": "First paragraph."
				},
				{
					"type": "text",
					"textType": "text",
					"content": "Second paragraph."
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Content, "First paragraph.")
	assert.Contains(t, msgs[0].Content, "Second paragraph.")
}

func TestParseCodebuffSession_TimestampVariants(t *testing.T) {
	tests := []struct {
		name     string
		ts       string
		wantZero bool
	}{
		{"HH:MM PM format", "03:04 PM", false},
		{"RFC3339", "2026-07-15T20:01:32Z", false},
		{"RFC3339Nano", "2026-07-15T20:01:32.065Z", false},
		{"empty string", "", true},
		{"garbage", "not-a-timestamp", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionDate := time.Date(2026, 7, 15, 0, 0, 0, 0, time.Local) //nolint:forbidigo // Exercise parsing of source timestamps recorded in local wall-clock time.
			ts := parseCodebuffTimestamp(tc.ts, sessionDate)
			if tc.wantZero {
				assert.True(t, ts.IsZero(), "expected zero time for %q", tc.ts)
			} else {
				assert.False(t, ts.IsZero(), "expected non-zero time for %q", tc.ts)
			}
		})
	}
}

func TestIsCodebuffTimestamp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// Four ISO-8601 shapes parseCodebuffSessionDate accepts.
		{"Z millis", "2026-07-16T00-09-00.236Z", true},
		{"Z no millis", "2026-07-16T00-09-00Z", true},
		{"no Z millis", "2026-07-16T00-09-00.123", true},
		{"date only", "2026-07-16", true},
		// Non-Codebuff shapes the generic resolver must keep open.
		{"Unix epoch numeric", "1704067200", false},
		{"UUID with dashes", "abcdef01-2345-6789-abcd-ef0123456789", false},
		{"empty", "", false},
		{"garbage", "not-a-timestamp", false},
		{"partial date", "2026-07", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsCodebuffTimestamp(tc.in))
		})
	}
}

func TestParseCodebuffSessionDate_Formats(t *testing.T) {
	tests := []struct {
		name    string
		session string
		wantNil bool
	}{
		{"Z suffix with millis", "2026-07-15T20-01-32.065Z", false},
		{"Z suffix without millis", "2026-07-15T20-01-32Z", false},
		{"no Z with millis", "2026-07-15T20-01-32.065", false},
		{"date only", "2026-07-15", false},
		{"garbage", "not-a-date", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ts := parseCodebuffSessionDate(tc.session)
			if tc.wantNil {
				assert.True(t, ts.IsZero())
			} else {
				assert.False(t, ts.IsZero())
			}
		})
	}
}

func TestParseCodebuffToolCall_UnknownName(t *testing.T) {
	// A tool block with empty toolName should return nil.
	raw := `{"type":"tool","toolName":"","toolCallId":"x","input":{}}`
	var block struct {
		Type       string         `json:"type"`
		ToolName   string         `json:"toolName"`
		ToolCallID string         `json:"toolCallId"`
		Input      jsontext.Value `json:"input"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &block))

	// parseCodebuffToolCall works on gjson.Result, so use the real path.
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "tool",
					"toolName": "",
					"toolCallId": "x"
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`
	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	// Empty toolName -> no tool call produced, so no HasToolUse message.
	for _, msg := range msgs {
		assert.False(t, msg.HasToolUse,
			"empty toolName should not produce a tool use message")
	}
}

func TestDiscoverCodebuffSessions(t *testing.T) {
	root := t.TempDir()

	// Create two projects with sessions.
	for _, proj := range []string{"proj-a", "proj-b"} {
		chatsDir := filepath.Join(root, proj, "chats")
		sessionDir := filepath.Join(chatsDir, "2026-07-15T20-01-32.065Z")
		require.NoError(t, os.MkdirAll(sessionDir, 0o755))
		codebuffWriteFile(t, filepath.Join(sessionDir, "chat-messages.json"), `[]`)
	}

	dirs := discoverCodebuffSessions(root)
	assert.Len(t, dirs, 2)

	projects := make(map[string]bool)
	for _, d := range dirs {
		projects[d.ProjectHint] = true
	}
	assert.True(t, projects["proj-a"])
	assert.True(t, projects["proj-b"])
}

func TestDiscoverCodebuffSessions_SkipsNonDirs(t *testing.T) {
	root := t.TempDir()
	// Create a file directly in root (not a directory).
	codebuffWriteFile(t, filepath.Join(root, "not-a-dir.txt"), "skip me")

	dirs := discoverCodebuffSessions(root)
	assert.Empty(t, dirs)
}

func TestCodebuffProjectFromPath(t *testing.T) {
	path := "/root/myproject/chats/2026-07-15T20-01-32.065Z/chat-messages.json"
	assert.Equal(t, "myproject", codebuffProjectFromPath(path))
}

func TestCodebuffProviderCapabilities(t *testing.T) {
	caps := codebuffProviderCapabilities()
	assert.Equal(t, CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(t, CapabilitySupported, caps.Content.SessionName)
	assert.Equal(t, CapabilitySupported, caps.Content.Thinking)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolResults)
	assert.Equal(t, CapabilityNotApplicable, caps.Content.Model,
		"model is unknown (selected server-side, can change mid-session)")
	assert.Equal(t, CapabilitySupported, caps.Content.AggregateUsageEvents)
	assert.Equal(t, CapabilitySupported, caps.Content.Relationships,
		"nested agent blocks become linked subagent sessions")
	assert.Equal(t, CapabilitySupported, caps.Source.MultiSessionSource,
		"a transcript holds the root session and its subagent sessions")
	assert.Equal(t, CapabilitySupported, caps.Content.TerminationStatus,
		"the last assistant turn's tool-call state classifies the ending")
	assert.Equal(t, CapabilityNotApplicable, caps.Content.MalformedLineCount)
}

func TestCodebuffSessionName_Truncation(t *testing.T) {
	var longPrompt strings.Builder
	for range 200 {
		longPrompt.WriteString("x")
	}
	chatMessages, err := json.Marshal([]map[string]any{
		{
			"id":        "user-1",
			"variant":   "user",
			"content":   longPrompt.String(),
			"timestamp": "03:04 PM",
		},
	})
	require.NoError(t, err)

	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, string(chatMessages), runState, "")
	sess, _, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Session name should be truncated to 80 chars with ellipsis.
	assert.LessOrEqual(t, len(sess.SessionName), 80)
	assert.Contains(t, sess.SessionName, "...")
}

func TestCodebuffSessionName_FallbackToProjectHint(t *testing.T) {
	chatMessages := `[]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	sess, _, _, err := parseCodebuffSession(dir, "my-hint", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// No messages, no firstPrompt -> falls back to project hint.
	assert.Equal(t, "my-hint", sess.SessionName)
}

func TestParseCodebuffSkills_Catalog(t *testing.T) {
	runState := `{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-free-minimax-m3"},
			"fileContext": {
				"skills": {
					"handoff": {
						"name": "handoff",
						"description": "Compact the conversation into a handoff doc.",
						"filePath": "/Users/dev/.skills/handoff/SKILL.md",
						"content": "---\nname: handoff\n---\nWrite a handoff."
					},
					"ponytail": {
						"name": "ponytail",
						"description": "Laziest solution that works."
					}
				}
			}
		}
	}`
	dir := codebuffTestSession(t, `[]`, runState, "")
	rs, err := readCodebuffRunState(filepath.Join(dir, "run-state.json"))
	require.NoError(t, err)
	require.Len(t, rs.Skills, 2)

	byName := map[string]codebuffSkill{}
	for _, s := range rs.Skills {
		byName[s.Name] = s
	}
	assert.Equal(t, "Compact the conversation into a handoff doc.",
		byName["handoff"].Description)
	assert.Equal(t, "/Users/dev/.skills/handoff/SKILL.md",
		byName["handoff"].FilePath)
	assert.Contains(t, byName["handoff"].Content, "Write a handoff.")
	assert.Equal(t, "Laziest solution that works.",
		byName["ponytail"].Description)
}

func TestParseCodebuffSkills_Empty(t *testing.T) {
	require.Empty(t, parseCodebuffSkills([]byte(`{"sessionState":{}}`)))
	require.Empty(t, parseCodebuffSkills([]byte(`not json`)))
}

func TestCodebuffAttachSkillNames_FromInput(t *testing.T) {
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "tool",
					"toolName": "run_terminal_command",
					"toolCallId": "tc-1",
					"input": {"command": "ponytail refactor parser.go"}
				},
				{
					"type": "tool",
					"toolName": "read_files",
					"toolCallId": "tc-2",
					"input": {"paths": ["src/main.go"]}
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-free-minimax-m3"},
			"fileContext": {
				"skills": {
					"ponytail": {"name": "ponytail", "description": "Lazy mode."}
				}
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	var toolMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].HasToolUse {
			toolMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolMsg)
	require.Len(t, toolMsg.ToolCalls, 2)

	assert.Equal(t, "ponytail", toolMsg.ToolCalls[0].SkillName,
		"tool call referencing a skill name should be attributed")
	assert.Empty(t, toolMsg.ToolCalls[1].SkillName,
		"unrelated tool call should not be attributed to a skill")
}

func TestCodebuffAttachSkillNames_ExplicitSkillTool(t *testing.T) {
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "tool",
					"toolName": "Skill",
					"toolCallId": "tc-1",
					"input": {"skill": "handoff"}
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-free-minimax-m3"},
			"fileContext": {
				"skills": {"handoff": {"name": "handoff", "description": "x"}}
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	var toolMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].HasToolUse {
			toolMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolMsg)
	require.Len(t, toolMsg.ToolCalls, 1)
	assert.Equal(t, "handoff", toolMsg.ToolCalls[0].SkillName)
}

func TestCodebuffAttachSkillNames_NoFalsePositiveSubstring(t *testing.T) {
	// A skill named "go" should NOT match inputs containing "going" or
	// "cargo" — the matching must use word boundaries, not substring.
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "tool",
					"toolName": "run_terminal_command",
					"toolCallId": "tc-1",
					"input": {"command": "going forward with refactor"}
				},
				{
					"type": "tool",
					"toolName": "run_terminal_command",
					"toolCallId": "tc-2",
					"input": {"command": "cargo build --release"}
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-free-minimax-m3"},
			"fileContext": {
				"skills": {
					"go": {"name": "go", "description": "Go skill."}
				}
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	var toolMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].HasToolUse {
			toolMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolMsg)
	require.Len(t, toolMsg.ToolCalls, 2)

	assert.Empty(t, toolMsg.ToolCalls[0].SkillName,
		"'going' should not match skill 'go'")
	assert.Empty(t, toolMsg.ToolCalls[1].SkillName,
		"'cargo' should not match skill 'go'")
}

func TestCodebuffAttachSkillNames_WordBoundaryMatch(t *testing.T) {
	// A skill named "go" SHOULD match when it appears as a standalone
	// word token in the input.
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "tool",
					"toolName": "run_terminal_command",
					"toolCallId": "tc-1",
					"input": {"command": "go build ./..."}
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-free-minimax-m3"},
			"fileContext": {
				"skills": {
					"go": {"name": "go", "description": "Go skill."}
				}
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	var toolMsg *ParsedMessage
	for i := range msgs {
		if msgs[i].HasToolUse {
			toolMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, toolMsg)
	require.Len(t, toolMsg.ToolCalls, 1)

	assert.Equal(t, "go", toolMsg.ToolCalls[0].SkillName,
		"'go build' should match skill 'go' as a standalone token")
}

func TestCodebuffToolCategories(t *testing.T) {
	tests := []struct {
		tool     string
		category string
	}{
		{"read_subtree", "Read"},
		{"file-picker", "Read"},
		{"read_files", "Read"},
		{"list_directory", "Read"},
		{"str_replace", "Edit"},
		{"write_file", "Write"},
		{"basher", "Bash"},
		{"spawn_agents", "Task"},
		{"suggest_followups", "Tool"},
		{"write_todos", "Tool"},
		{"read_url", "Tool"},
		{"ask_user", "Tool"},
		{"render_ui", "Tool"},
		{"gravity_index", "Tool"},
		{"skill", "Tool"},
		{"code-searcher", "Tool"},
		{"code-reviewer", "Tool"},
	}
	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			got := NormalizeToolCategory(tc.tool)
			assert.Equalf(t, tc.category, got, "NormalizeToolCategory(%q)", tc.tool)
		})
	}
}

func TestParseCodebuffSession_ErrorVariant(t *testing.T) {
	chatMessages := `[
		{
			"id": "user-1",
			"variant": "user",
			"content": "Fix the bug",
			"timestamp": "03:04 PM"
		},
		{
			"id": "err-1",
			"variant": "error",
			"content": "Rate limit exceeded. Please try again later.",
			"timestamp": "03:05 PM"
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	// Should have user message + error system message.
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Fix the bug", msgs[0].Content)

	assert.Equal(t, RoleSystem, msgs[1].Role)
	assert.True(t, msgs[1].IsSystem)
	assert.Contains(t, msgs[1].Content, "Rate limit exceeded")
}

func TestParseCodebuffSessionTurnCostTable(t *testing.T) {
	ai := func(id, creditsRaw string) string {
		credits := ""
		if creditsRaw != "" {
			credits = `"credits":` + creditsRaw + `,`
		}
		return `{"id":"` + id + `","variant":"ai","timestamp":"03:04 PM",` + credits +
			`"blocks":[{"type":"text","textType":"text","content":"done for ` + id + `"}]}`
	}
	const user = `{"id":"user-1","variant":"user","content":"hi","timestamp":"03:04 PM"}`
	runState := func(agentType, creditsUsed string) string {
		return `{"sessionState":{"mainAgentState":{"agentType":"` + agentType +
			`","creditsUsed":` + creditsUsed + `}}}`
	}
	const turnKey = "turn:codebuff:p:001:"
	const sessionKey = "session:codebuff:p:001"

	tests := []struct {
		name       string
		transcript string
		runState   string
		wantMicros []int64
		wantKeys   []string
	}{
		{
			name:       "one billed turn",
			transcript: "[" + ai("ai-1", "150") + "]",
			runState:   runState("base2-deepseek", "0"),
			wantMicros: []int64{1_500_000},
			wantKeys:   []string{turnKey + "ai-1"},
		},
		{
			name:       "decimal credits",
			transcript: "[" + ai("ai-1", "0.5") + "]",
			runState:   runState("base2-deepseek", "0"),
			wantMicros: []int64{5_000},
			wantKeys:   []string{turnKey + "ai-1"},
		},
		{
			name:       "each billed turn is its own row and creditsUsed is ignored",
			transcript: "[" + ai("ai-1", "1") + "," + ai("ai-2", "1") + "," + ai("ai-3", "1") + "]",
			runState:   runState("base2-deepseek", "999"),
			wantMicros: []int64{10_000, 10_000, 10_000},
			wantKeys:   []string{turnKey + "ai-1", turnKey + "ai-2", turnKey + "ai-3"},
		},
		{
			name:       "unbilled message and in-flight creditsUsed add nothing",
			transcript: "[" + ai("ai-1", "10") + "," + ai("ai-2", "") + "," + ai("ai-3", "10") + "]",
			runState:   runState("base2-deepseek", "33"),
			wantMicros: []int64{100_000, 100_000},
			wantKeys:   []string{turnKey + "ai-1", turnKey + "ai-3"},
		},
		{
			name:       "present zero suppresses legacy fallback",
			transcript: "[" + ai("ai-1", "0") + "]",
			runState:   runState("base2-deepseek", "42"),
		},
		{
			name:       "negative and non-numeric credits emit nothing",
			transcript: "[" + ai("ai-1", "-5") + "," + ai("ai-2", `"abc"`) + "," + ai("ai-3", "null") + "]",
			runState:   runState("base2-deepseek", "42"),
		},
		{
			name:       "unresolvable model emits nothing per turn",
			transcript: "[" + ai("ai-1", "150") + "]",
			runState:   runState("", "0"),
		},
		{
			name:       "legacy transcript keeps single session row",
			transcript: "[" + user + "]",
			runState:   runState("base2-deepseek", "15.5"),
			wantMicros: []int64{155_000},
			wantKeys:   []string{sessionKey},
		},
		{
			name:       "legacy zero credits emits nothing",
			transcript: "[" + user + "]",
			runState:   runState("base2-deepseek", "0"),
		},
		{
			name:       "legacy unresolvable model emits nothing",
			transcript: "[" + user + "]",
			runState:   runState("", "42"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := codebuffTestSession(t, tc.transcript, tc.runState, "")
			sess, _, _, err := parseCodebuffSession(dir, "p", "local")
			require.NoError(t, err)

			var micros []int64
			var keys []string
			for _, evt := range sess.UsageEvents {
				require.NotNil(t, evt.Cost)
				micros = append(micros, evt.Cost.Microdollars)
				keys = append(keys, evt.DedupKey)
				assert.Equal(t, "base2-deepseek", evt.Model)
				assert.Equal(t, "reported", evt.CostStatus)
				assert.NotEqual(t, time.Time{}.Format(time.RFC3339Nano), evt.OccurredAt)
				if evt.DedupKey == sessionKey {
					assert.Nil(t, evt.MessageOrdinal)
				} else {
					assert.NotNil(t, evt.MessageOrdinal)
				}
			}
			assert.Equal(t, tc.wantMicros, micros)
			assert.Equal(t, tc.wantKeys, keys)
		})
	}
}

func TestParseCodebuffSessionTurnModelResolution(t *testing.T) {
	const billedTurn = `{"id":"ai-1","variant":"ai","timestamp":"03:04 PM","credits":10%s}`
	const legacyTranscript = `[{"id":"user-1","variant":"user","content":"hi","timestamp":"03:04 PM"}]`
	const hostOnly = `{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}`

	tests := []struct {
		name       string
		msgState   string // message metadata.runState; empty for none
		fileState  string // run-state.json; empty for none
		transcript string // overrides the billed turn when set
		want       string
	}{
		{
			name: "byok inference model wins",
			msgState: `{"inference":{"source":"byok","model":"claude-sonnet-4"},
				"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}`,
			fileState: hostOnly,
			want:      "claude-sonnet-4",
		},
		{
			name: "hosted run resolves through agentTemplates",
			msgState: `{"inference":{"source":"codebuff"},"sessionState":{
				"mainAgentState":{"agentType":"base2-deepseek"},
				"fileContext":{"agentTemplates":{"base2-deepseek":{"model":"deepseek-v3"}}}}}`,
			fileState: hostOnly,
			want:      "deepseek-v3",
		},
		{
			name: "template id containing path metacharacters",
			msgState: `{"sessionState":{"mainAgentState":{"agentType":"team.base*2"},
				"fileContext":{"agentTemplates":{"team.base*2":{"model":"deepseek-v3"}}}}}`,
			want: "deepseek-v3",
		},
		{
			name: "no message metadata falls back to run-state.json",
			fileState: `{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"},
				"fileContext":{"agentTemplates":{"base2-deepseek":{"model":"deepseek-chat"}}}}}`,
			want: "deepseek-chat",
		},
		{
			name:      "run-state.json byok is reached past a bare message agentType",
			msgState:  hostOnly,
			fileState: `{"inference":{"source":"byok","model":"claude-sonnet-4"}}`,
			want:      "claude-sonnet-4",
		},
		{
			name:      "unresolvable everywhere falls back to template id",
			fileState: hostOnly,
			want:      "base2-deepseek",
		},
		{
			name:     "message agentType applies without run-state.json",
			msgState: hostOnly,
			want:     "base2-deepseek",
		},
		{
			name: "legacy path uses run-state template model",
			fileState: `{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek","creditsUsed":42},
				"fileContext":{"agentTemplates":{"base2-deepseek":{"model":"deepseek-v3"}}}}}`,
			transcript: legacyTranscript,
			want:       "deepseek-v3",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transcript := tc.transcript
			if transcript == "" {
				meta := ""
				if tc.msgState != "" {
					meta = `,"metadata":{"runState":` + tc.msgState + `}`
				}
				transcript = "[" + fmt.Sprintf(billedTurn, meta) + "]"
			}
			dir := codebuffTestSession(t, transcript, tc.fileState, "")
			sess, _, _, err := parseCodebuffSession(dir, "p", "local")
			require.NoError(t, err)
			require.Len(t, sess.UsageEvents, 1)
			assert.Equal(t, tc.want, sess.UsageEvents[0].Model)
		})
	}
}

func TestParseCodebuffSession_PlanBlock(t *testing.T) {
	chatMessages := `[
		{
			"id": "ai-1",
			"variant": "ai",
			"content": "",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "plan",
					"content": "1. Fix the auth handler\n2. Add tests\n3. Update docs"
				},
				{
					"type": "text",
					"textType": "text",
					"content": "I'll implement the plan."
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	// Should have a system message with the plan content.
	found := false
	for _, msg := range msgs {
		if msg.IsSystem && strings.Contains(msg.Content, "[Plan]") {
			found = true
			assert.Contains(t, msg.Content, "Fix the auth handler")
			break
		}
	}
	assert.True(t, found, "expected a system message with plan content")
}

func TestParseCodebuffSession_ImageBlock(t *testing.T) {
	chatMessages := `[
		{
			"id": "user-1",
			"variant": "user",
			"content": "Here's a screenshot",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "image",
					"image": "base64data...",
					"mediaType": "image/png",
					"filename": "screenshot.png"
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	// User message should contain the image filename note.
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Content, "[Image: screenshot.png]")
}

func TestParseCodebuffSession_ImageBlockNoFilename(t *testing.T) {
	chatMessages := `[
		{
			"id": "user-1",
			"variant": "user",
			"content": "Look at this",
			"timestamp": "03:04 PM",
			"blocks": [
				{
					"type": "image",
					"image": "base64data...",
					"mediaType": "image/png"
				}
			]
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {
				"agentType": "base2-deepseek"
			}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)

	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Content, "[Image attached]")
}

func TestAgentByPrefix_FreebuffAlias(t *testing.T) {
	// Freebuff sessions use the "freebuff:" prefix but share the Codebuff
	// provider. AgentByPrefix must resolve them to the Codebuff definition
	// with the freebuff: prefix so callers can strip it correctly.
	def, ok := AgentByPrefix("freebuff:2026-07-15T20-01-32.065Z")
	require.True(t, ok, "AgentByPrefix should resolve freebuff: prefix")
	assert.Equal(t, AgentCodebuff, def.Type,
		"freebuff: prefix should map to Codebuff provider")
	assert.Equal(t, "freebuff:", def.IDPrefix,
		"IDPrefix should match the freebuff: prefix for correct stripping")
}

// TestParseCodebuffMixedFormatMidnightRollover pins the regression
// flagged by roborev review at internal/parser/codebuff.go:496: a
// non-time-only (RFC3339) timestamp must anchor currentDate to
// its local calendar date so subsequent time-only messages don't
// get assigned to the previous session directory date across
// midnight. Before this fix the non-time-only path reset
// prevHour to -1 without advancing currentDate, causing the
// parser to drift back to the session directory's original date
// for any time-only message that followed a real boundary.
func TestParseCodebuffMixedFormatMidnightRollover(t *testing.T) {
	// Pin time.Local to UTC so the session-date parse and the
	// time-only message reconstruction are deterministic regardless
	// of the host TZ. t.Setenv only triggers a TZ reload on the next
	// time.Local access, and earlier tests in the package can cache
	// the location before this runs; assigning time.Local directly
	// and restoring it on cleanup is the only reliable way to pin
	// the state for this single test without touching package init.
	origLocal := time.Local                      //nolint:forbidigo // Exercise parsing of source timestamps recorded in local wall-clock time.
	t.Cleanup(func() { time.Local = origLocal }) //nolint:forbidigo // Exercise parsing of source timestamps recorded in local wall-clock time.
	time.Local = time.UTC                        //nolint:forbidigo // Exercise parsing of source timestamps recorded in local wall-clock time.

	sessionID := "2026-07-15T22-00-00.000Z"
	sessionDate := parseCodebuffSessionDate(sessionID)
	require.Equal(t, 2026, sessionDate.Year())
	require.Equal(t, time.July, sessionDate.Month())
	require.Equal(t, 15, sessionDate.Day())
	require.Equal(t, 22, sessionDate.Hour())

	data := strings.NewReader(`[
		{"id":"u1","variant":"user","content":"hello","timestamp":"2026-07-15T22:00:00Z"},
		{"id":"u2","variant":"user","content":"ack","timestamp":"11:30 PM"},
		{"id":"u3","variant":"user","content":"next","timestamp":"12:15 AM"},
		{"id":"u4","variant":"user","content":"later","timestamp":"2026-07-17T09:00:00Z"},
		{"id":"u5","variant":"user","content":"after","timestamp":"02:00 PM"}
	]`)

	out, err := decodeCodebuffMessages(data, sessionDate, "")
	require.NoError(t, err)
	msgs := out.Messages
	require.Len(t, msgs, 5)

	// M1 — RFC3339 anchor; date must be 15, hour 22.
	require.Equal(t, 2026, msgs[0].Timestamp.Year())
	require.Equal(t, time.July, msgs[0].Timestamp.Month())
	require.Equal(t, 15, msgs[0].Timestamp.Day())
	require.Equal(t, 22, msgs[0].Timestamp.Hour())

	// M2 — time-only on date 15; hour 23.
	require.Equal(t, 15, msgs[1].Timestamp.Day())
	require.Equal(t, 23, msgs[1].Timestamp.Hour())

	// M3 — time-only after midnight rollover; date 16, hour 0.
	// Before the fix currentDate would still be 15 here and M3
	// would land on 15 00:15 instead of 16 00:15.
	require.Equal(t, 16, msgs[2].Timestamp.Day(),
		"time-only after a 23:xx anchor must advance to the next calendar date")
	require.Equal(t, 0, msgs[2].Timestamp.Hour())

	// M4 — RFC3339 anchor that crosses a real midnight; date 17.
	require.Equal(t, 17, msgs[3].Timestamp.Day(),
		"non-time-only timestamp must anchor currentDate to its local calendar date")

	// M5 — time-only on date 17; hour 14. Before the fix
	// currentDate could regress to 15 here because M4 only
	// touched prevHour.
	require.Equal(t, 17, msgs[4].Timestamp.Day(),
		"time-only after an RFC3339 anchor that crossed a date boundary must stay on the new date")
	require.Equal(t, 14, msgs[4].Timestamp.Hour())
}

// TestParseCodebuffMessages_FirstMessageMidnightRollover pins the
// rollover seed for the very first message: a session created late in
// the local evening (directory 2026-07-17T06-58-00.000Z = 23:58 on
// July 16 in UTC-7) whose first time-only message reads "12:01 AM"
// belongs to the next local calendar day (July 17), not ~24 hours
// before the session started. Before the fix prevHour started at -1,
// so the first message could never roll over midnight and was stamped
// July 16 00:01, skewing StartedAt.
func TestParseCodebuffMessages_FirstMessageMidnightRollover(t *testing.T) {
	// Pin time.Local to a fixed UTC-7 zone so the session-date parse
	// and the time-only reconstruction are deterministic regardless of
	// the host TZ (same pinning approach as the mixed-format rollover
	// test above).
	origLocal := time.Local                       //nolint:forbidigo // Exercise parsing of source timestamps recorded in local wall-clock time.
	t.Cleanup(func() { time.Local = origLocal })  //nolint:forbidigo // Exercise parsing of source timestamps recorded in local wall-clock time.
	time.Local = time.FixedZone("UTC-7", -7*3600) //nolint:forbidigo // Exercise parsing of source timestamps recorded in local wall-clock time.

	sessionDate := parseCodebuffSessionDate("2026-07-17T06-58-00.000Z")
	require.Equal(t, 16, sessionDate.Day(), "06:58 UTC is 23:58 on July 16 in UTC-7")
	require.Equal(t, 23, sessionDate.Hour())

	data := strings.NewReader(`[
		{"id":"u1","variant":"user","content":"hello","timestamp":"12:01 AM"},
		{"id":"u2","variant":"user","content":"more","timestamp":"12:30 AM"}
	]`)

	out, err := decodeCodebuffMessages(data, sessionDate, "")
	require.NoError(t, err)
	msgs, startedAt := out.Messages, out.StartedAt
	require.Len(t, msgs, 2)

	assert.Equal(t, 17, msgs[0].Timestamp.Day(),
		"first time-only message past local midnight must land on the next calendar day")
	assert.Equal(t, 0, msgs[0].Timestamp.Hour())
	assert.Equal(t, 1, msgs[0].Timestamp.Minute())
	assert.Equal(t, 17, msgs[1].Timestamp.Day())
	assert.Equal(t, 17, startedAt.Day(),
		"StartedAt must not be skewed ~24h before the session directory timestamp")
}

// TestCodebuffAttachSkillNames_DeterministicFallbackWinner pins two
// contracts of the input-scan skill attribution: when two catalog
// skills both appear as tokens in the tool input, the winner is
// deterministic (first in lowercase-sorted order), and both the
// direct-key path and the fallback token scan return the catalog's
// canonical casing rather than the input's or the lowercased key.
func TestCodebuffAttachSkillNames_DeterministicFallbackWinner(t *testing.T) {
	msgs := []ParsedMessage{{
		Role:       RoleAssistant,
		HasToolUse: true,
		ToolCalls: []ParsedToolCall{
			{
				ToolUseID: "tc-1",
				ToolName:  "run_terminal_command",
				InputJSON: `{"command":"zulu-skill then alpha-skill"}`,
			},
			{
				ToolUseID: "tc-2",
				ToolName:  "run_terminal_command",
				InputJSON: `{"command":"alpha-skill"}`,
			},
		},
	}}
	skills := []codebuffSkill{
		{Name: "Zulu-Skill", Description: "z"},
		{Name: "Alpha-Skill", Description: "a"},
	}
	codebuffAttachSkillNames(msgs, skills)

	assert.Equal(t, "Alpha-Skill", msgs[0].ToolCalls[0].SkillName,
		"two-match input must pick the lowercase-sorted first skill with catalog casing")
	assert.Equal(t, "Alpha-Skill", msgs[0].ToolCalls[1].SkillName,
		"direct command-key match must return the catalog's canonical casing")
}

// TestParseCodebuffSession_SessionNameRuneSafeTruncation pins that
// the 80-byte session-name shortening cannot split a multi-byte rune:
// a first message of 100 two-byte runes must yield a valid-UTF-8 name
// of the first 77 runes plus "...", not a raw 77-byte slice ending in
// half a rune.
func TestParseCodebuffSession_SessionNameRuneSafeTruncation(t *testing.T) {
	longPrompt := strings.Repeat("é", 100)
	chatMessages := `[
		{
			"id": "user-1",
			"variant": "user",
			"content": "` + longPrompt + `",
			"timestamp": "2026-07-15T15:04:00Z"
		}
	]`
	runState := `{
		"sessionState": {
			"mainAgentState": {"agentType": "base2-deepseek"}
		}
	}`

	dir := codebuffTestSession(t, chatMessages, runState, "")
	sess, _, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.True(t, utf8.ValidString(sess.SessionName),
		"session name must remain valid UTF-8 after truncation")
	assert.Equal(t, strings.Repeat("é", 77)+"...", sess.SessionName)
}

// TestParseCodebuffSessionAgentVariant covers variant routing: agent and
// unknown future variants render as assistant content.
func TestParseCodebuffSessionAgentVariant(t *testing.T) {
	runState := `{"sessionState":{"mainAgentState":{"agentType":"base2-free-mimo"}}}`

	t.Run("agent content renders as assistant", func(t *testing.T) {
		dir := codebuffTestSession(t,
			`[{"id":"ag-1","variant":"agent","content":"subagent finished the task","timestamp":"03:04 PM"}]`,
			runState, "")
		sess, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.Equal(t, RoleAssistant, msgs[0].Role)
		assert.Equal(t, "subagent finished the task", msgs[0].Content)
		assert.False(t, msgs[0].IsSystem)
		assert.Empty(t, sess.UsageEvents)
	})

	t.Run("agent empty content produces nothing", func(t *testing.T) {
		dir := codebuffTestSession(t,
			`[{"id":"ag-1","variant":"agent","content":"","timestamp":"03:04 PM"}]`,
			runState, "")
		_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		assert.Empty(t, msgs)
	})

	t.Run("unknown variant renders like agent", func(t *testing.T) {
		dir := codebuffTestSession(t,
			`[{"id":"x-1","variant":"workflow-step","content":"from a future build","timestamp":"03:04 PM"}]`,
			runState, "")
		_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.Equal(t, RoleAssistant, msgs[0].Role)
		assert.Equal(t, "from a future build", msgs[0].Content)
	})
}

// TestParseCodebuffSessionUserErrorEnvelope covers the runtime-error
// envelope: userError is a notice the app displayed, so it emits its own
// system message and never merges into the user message's Content.
func TestParseCodebuffSessionUserErrorEnvelope(t *testing.T) {
	runState := `{"sessionState":{"mainAgentState":{"agentType":"base2-free-mimo"}}}`
	transcript := `[
		{"id":"u1","variant":"user","content":"go ahead","timestamp":"03:04 PM",
		 "userError":"connection dropped mid-run"}
	]`
	dir := codebuffTestSession(t, transcript, runState, "")
	sess, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "go ahead", msgs[0].Content,
		"the user message's own content must stay untouched")
	assert.Equal(t, RoleSystem, msgs[1].Role)
	assert.True(t, msgs[1].IsSystem)
	assert.Equal(t, "connection dropped mid-run", msgs[1].Content)
	assert.Equal(t, 0, sess.MalformedLines)
}

// TestParseCodebuffSessionValidationErrors covers one system message per
// validation entry.
func TestParseCodebuffSessionValidationErrors(t *testing.T) {
	runState := `{"sessionState":{"mainAgentState":{"agentType":"base2-free-mimo"}}}`
	transcript := `[
		{"id":"u1","variant":"user","content":"hi","timestamp":"03:04 PM",
		 "validationErrors":[
			{"id":"v1","message":"first failure"},
			{"id":"v2","message":"second failure"}
		 ]}
	]`
	dir := codebuffTestSession(t, transcript, runState, "")
	_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	assert.Equal(t, "[Validation error] first failure", msgs[1].Content)
	assert.Equal(t, "[Validation error] second failure", msgs[2].Content)
	assert.True(t, msgs[1].IsSystem && msgs[2].IsSystem)
}

// TestParseCodebuffSessionAttachmentEnvelope covers image, pasted-text, and
// file attachment markers, the privacy exclusions (no paths, no full pasted
// content), and the envelope-only message counting as a user message.
func TestParseCodebuffSessionAttachmentEnvelope(t *testing.T) {
	runState := `{"sessionState":{"mainAgentState":{"agentType":"base2-free-mimo"}}}`

	t.Run("image attachments with and without filename", func(t *testing.T) {
		transcript := `[
			{"id":"u1","variant":"user","content":"look","timestamp":"03:04 PM",
			 "attachments":[{"filename":"shot.png","path":"/home/x/Pictures/shot.png"},{"path":"/tmp/no-name.png"}]}
		]`
		dir := codebuffTestSession(t, transcript, runState, "")
		_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.Contains(t, msgs[0].Content, "[Image: shot.png]")
		assert.Contains(t, msgs[0].Content, "[Image attached]")
		assert.NotContains(t, msgs[0].Content, "/home/x", "local paths are never stored")
		assert.NotContains(t, msgs[0].Content, "/tmp/")
	})

	t.Run("text attachment stores label and preview only", func(t *testing.T) {
		full := strings.Repeat("pasted ", 500) + "SECRET-TAIL"
		contentJSON, err := json.Marshal(full)
		require.NoError(t, err)
		previewJSON, err := json.Marshal(full[:300])
		require.NoError(t, err)
		transcript := fmt.Sprintf(
			`[{"id":"u1","variant":"user","content":"","timestamp":"03:04 PM",
			  "textAttachments":[{"id":"t1","content":%s,"preview":%s,"charCount":3500}]}]`,
			contentJSON, previewJSON,
		)
		dir := codebuffTestSession(t, transcript, runState, "")
		_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		lines := strings.Split(msgs[0].Content, "\n")
		require.Len(t, lines, 2)
		assert.Equal(t, "[Text attachment: 3500 chars]", lines[0])
		assert.Equal(t, full[:197]+"…", lines[1])
	})

	t.Run("file attachments plain, directory, and with note", func(t *testing.T) {
		transcript := `[
			{"id":"u1","variant":"user","content":"","timestamp":"03:04 PM",
			 "fileAttachments":[
				{"path":"/home/x/report.pdf","filename":"report.pdf","isDirectory":false},
				{"path":"/home/x/src","filename":"src","isDirectory":true},
				{"path":"/home/x/notes.md","filename":"notes.md","isDirectory":false,"note":"edited today"}
			 ]}
		]`
		dir := codebuffTestSession(t, transcript, runState, "")
		_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.Contains(t, msgs[0].Content, "[File: report.pdf]")
		assert.Contains(t, msgs[0].Content, "[File: src] (directory)")
		assert.Contains(t, msgs[0].Content, "[File: notes.md] edited today")
		assert.NotContains(t, msgs[0].Content, "/home/x", "local paths are never stored")
	})

	t.Run("envelope-only user message counts as a user message", func(t *testing.T) {
		transcript := `[
			{"id":"u1","variant":"user","content":"","timestamp":"03:04 PM",
			 "attachments":[{"filename":"only.png"}]}
		]`
		dir := codebuffTestSession(t, transcript, runState, "")
		sess, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.Equal(t, RoleUser, msgs[0].Role)
		assert.Equal(t, 1, sess.UserMessageCount,
			"upstream counts the message, not the text")
		assert.Equal(t, 1, sess.MessageCount)
	})
}

func TestParseCodebuffSessionAskUser(t *testing.T) {
	const questions = `"questions":[
		{"question":"Which database?","header":"Database","options":[{"label":"Postgres"}]},
		{"question":"long question long question long question"}]`
	tests := []struct {
		name  string
		block string
		want  string
	}{
		{
			name:  "unanswered question",
			block: `{"type":"ask-user",` + questions + `}`,
			want:  "[Agent asked] Which database?\n[Agent asked] long question long question long question",
		},
		{
			name: "selected option labeled by header",
			block: `{"type":"ask-user",` + questions + `,
				"answers":[{"questionIndex":0,"selectedOption":"Postgres"}]}`,
			want: "[Agent asked] Which database?\n[Agent asked] long question long question long question\n" +
				"[Answer: Database] Postgres",
		},
		{
			name: "multi-option, headerless label, and out-of-range index",
			block: `{"type":"ask-user",` + questions + `,
				"answers":[{"questionIndex":1,"selectedOptions":["a","b"]},
					{"questionIndex":9,"otherText":"none of these"}]}`,
			want: "[Agent asked] Which database?\n[Agent asked] long question long question long question\n" +
				"[Answer: long question long question long question] a, b\n" +
				"[Answer] none of these",
		},
		{
			name:  "skipped block",
			block: `{"type":"ask-user",` + questions + `,"skipped":true}`,
			want: "[Agent asked] Which database?\n[Agent asked] long question long question long question\n" +
				"[Skipped]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transcript := `[{"id":"ai-1","variant":"ai","timestamp":"03:04 PM","blocks":[` + tc.block + `]}]`
			dir := codebuffTestSession(t, transcript, "", "")
			_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			assert.Equal(t, RoleSystem, msgs[0].Role)
			assert.Equal(t, tc.want, msgs[0].Content)
		})
	}
}

// TestParseCodebuffSessionSponsoredAndAgentList covers the two new block
// types and their privacy exclusions.
func TestParseCodebuffSessionSponsoredAndAgentList(t *testing.T) {
	runState := `{"sessionState":{"mainAgentState":{"agentType":"base2-free-mimo"}}}`

	t.Run("sponsored proposal with consent", func(t *testing.T) {
		transcript := `[
			{"id":"ai-1","variant":"ai","timestamp":"03:04 PM","credits":1,
			 "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-free-mimo"}}}},
			 "blocks":[{
				"type":"sponsored-proposal",
				"proposal":{"id":"p1","name":"Super Deal"},
				"target":"this repository",
				"consent":{"advertiserName":"AdCo","headline":"Try AdCo Free",
					"body":"long sales pitch","folder":"/home/x/ads",
					"branch":"secret-branch","runId":"run-123"}
			 }]}]
		`
		dir := codebuffTestSession(t, transcript, runState, "")
		_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.Equal(t, RoleSystem, msgs[0].Role,
			"a sponsored-proposal notice is a record, not agent speech")
		assert.True(t, msgs[0].IsSystem)
		assert.Contains(t, msgs[0].Content, "[Sponsored proposal] this repository")
		assert.Contains(t, msgs[0].Content, "Try AdCo Free")
		assert.NotContains(t, msgs[0].Content, "Super Deal", "the proposal payload is never stored")
		assert.NotContains(t, msgs[0].Content, "long sales pitch")
		assert.NotContains(t, msgs[0].Content, "/home/x/ads")
		assert.NotContains(t, msgs[0].Content, "secret-branch")
		assert.NotContains(t, msgs[0].Content, "run-123")
	})

	t.Run("agent list renders display names, never agentsDir", func(t *testing.T) {
		transcript := `[
			{"id":"ai-1","variant":"ai","timestamp":"03:04 PM","credits":1,
			 "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-free-mimo"}}}},
			 "blocks":[{
				"type":"agent-list",
				"id":"al-1",
				"agents":[{"id":"a1","displayName":"Reviewer"},{"id":"a2"}],
				"agentsDir":"/home/x/.codebuff/agents"
			 }]}]
		`
		dir := codebuffTestSession(t, transcript, runState, "")
		_, msgs, _, err := parseCodebuffSession(dir, "p", "local")
		require.NoError(t, err)
		require.Len(t, msgs, 1)
		assert.Equal(t, RoleSystem, msgs[0].Role,
			"an agent-list roster is a record, not agent speech")
		assert.True(t, msgs[0].IsSystem)
		assert.Equal(t, "[Agents: Reviewer, a2]", msgs[0].Content,
			"display name preferred, id fallback, order preserved")
		assert.NotContains(t, msgs[0].Content, "/home/x", "agentsDir is never stored")
	})
}

// --- nested subagents as linked sessions ---

// codebuffSubagentTranscript holds one AI message whose agent block did
// visible work (text, a read_files call with output) and spawned a nested
// agent of its own.
const codebuffSubagentTranscript = `[
	{"id":"u1","variant":"user","content":"look around","timestamp":"03:03 PM"},
	{"id":"ai-1","variant":"ai","timestamp":"03:04 PM","credits":3,
	 "metadata":{"runState":{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"}}}},
	 "blocks":[
		{"type":"agent","agentId":"agent-1","agentName":"Explorer","agentType":"file-explorer",
		 "status":"complete","initialPrompt":"find the config","content":"config is in cfg.toml",
		 "blocks":[
			{"type":"text","textType":"text","content":"reading the config"},
			{"type":"tool","toolName":"read_files","toolCallId":"rf-1",
			 "input":{"paths":["cfg.toml"]},"output":"SECRET FILE CONTENTS"},
			{"type":"agent","agentId":"agent-2","agentName":"Picker","agentType":"file-picker",
			 "status":"complete","content":"picked",
			 "blocks":[{"type":"text","textType":"text","content":"picking files"}]}
		 ]}
	 ]}
]`

// codebuffParseProviderDir parses one session directory laid out under a
// provider root (<root>/<project>/chats/<ts>/) through the registered
// Codebuff provider, the path sync uses.
func codebuffParseProviderDir(
	t *testing.T, chatMessages string,
) (ParseOutcome, string) {
	t.Helper()
	root := t.TempDir()
	ts := "2026-07-15T20-01-32.065Z"
	sessionDir := filepath.Join(root, "proj", "chats", ts)
	codebuffWriteFile(t, filepath.Join(sessionDir, "chat-messages.json"), chatMessages)
	codebuffWriteFile(t, filepath.Join(sessionDir, "run-state.json"),
		`{"sessionState":{"mainAgentState":{"agentType":"base2-deepseek"},"fileContext":{"cwd":"/work/proj"}}}`)

	var factory ProviderFactory
	for _, f := range ProviderFactories() {
		if f.Definition().Type == AgentCodebuff {
			factory = f
		}
	}
	require.NotNil(t, factory)
	provider := factory.NewProvider(ProviderConfig{Roots: []string{root}, Machine: "local"})
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source: sources[0], Fingerprint: fingerprint, Machine: "local",
	})
	require.NoError(t, err)
	return outcome, "codebuff:proj:" + ts
}

// codebuffResultByID returns the parse result for one full session ID.
func codebuffResultByID(
	t *testing.T, outcome ParseOutcome, id string,
) ParseResult {
	t.Helper()
	for _, r := range outcome.Results {
		if r.Result.Session.ID == id {
			return r.Result
		}
	}
	require.Failf(t, "missing session", "no result with ID %s", id)
	return ParseResult{}
}

func codebuffAllToolCalls(msgs []ParsedMessage) []ParsedToolCall {
	var out []ParsedToolCall
	for _, m := range msgs {
		out = append(out, m.ToolCalls...)
	}
	return out
}

func codebuffAllToolResults(msgs []ParsedMessage) []ParsedToolResult {
	var out []ParsedToolResult
	for _, m := range msgs {
		out = append(out, m.ToolResults...)
	}
	return out
}

func TestCodebuffSubagentsBecomeLinkedSessions(t *testing.T) {
	outcome, parentID := codebuffParseProviderDir(t, codebuffSubagentTranscript)
	childID := parentID + "__subagent__agent-1"
	grandchildID := parentID + "__subagent__agent-2"

	require.Len(t, outcome.Results, 3, "parent, child, and grandchild")
	assert.True(t, outcome.ForceReplace)
	assert.Equal(t, parentID, outcome.Results[0].Result.Session.ID,
		"the parent is emitted before its subagents")

	parent := codebuffResultByID(t, outcome, parentID)
	child := codebuffResultByID(t, outcome, childID)
	grandchild := codebuffResultByID(t, outcome, grandchildID)

	// The parent's Task call links to the child; its result is only the
	// subagent's final answer.
	calls := codebuffAllToolCalls(parent.Messages)
	require.Len(t, calls, 1)
	assert.Equal(t, "Task", calls[0].Category)
	assert.Equal(t, childID, calls[0].SubagentSessionID)
	results := codebuffAllToolResults(parent.Messages)
	require.Len(t, results, 1)
	assert.Equal(t, "config is in cfg.toml", DecodeContent(results[0].ContentRaw))
	for _, m := range parent.Messages {
		assert.NotContains(t, m.Content, "SECRET FILE CONTENTS")
	}
	assert.Len(t, parent.UsageEvents, 1, "credits stay on the parent")

	// The child is a linked subagent session holding the nested work.
	assert.Equal(t, parentID, child.Session.ParentSessionID)
	assert.Equal(t, RelSubagent, child.Session.RelationshipType)
	assert.Equal(t, "Subagent: Explorer", child.Session.SessionName)
	assert.Equal(t, "agent-1", child.Session.SourceSessionID)
	assert.Empty(t, child.UsageEvents)

	require.NotEmpty(t, child.Messages)
	assert.Equal(t, RoleUser, child.Messages[0].Role)
	assert.Equal(t, "find the config", child.Messages[0].Content)
	childCalls := codebuffAllToolCalls(child.Messages)
	require.Len(t, childCalls, 2)
	assert.Equal(t, "read_files", childCalls[0].ToolName)
	assert.Equal(t, "Read", childCalls[0].Category)
	assert.Equal(t, "rf-1", childCalls[0].ToolUseID)
	assert.Equal(t, "Task", childCalls[1].Category)
	assert.Equal(t, grandchildID, childCalls[1].SubagentSessionID)
	childResults := codebuffAllToolResults(child.Messages)
	require.Len(t, childResults, 2)
	assert.Equal(t, "rf-1", childResults[0].ToolUseID)
	assert.Equal(t, "SECRET FILE CONTENTS", DecodeContent(childResults[0].ContentRaw))

	// The nested agent inside the subagent links to the subagent.
	assert.Equal(t, childID, grandchild.Session.ParentSessionID)
	assert.Equal(t, RelSubagent, grandchild.Session.RelationshipType)
	require.Len(t, grandchild.Messages, 1)
	assert.Equal(t, "picking files", grandchild.Messages[0].Content)

	for _, r := range outcome.Results {
		for i, m := range r.Result.Messages {
			assert.Equal(t, i, m.Ordinal, "%s ordinals are sequential", r.Result.Session.ID)
		}
	}
}

func TestCodebuffSubagentIDAllocation(t *testing.T) {
	agent := func(id string) string {
		return `{"type":"agent","agentId":` + strconv.Quote(id) +
			`,"agentType":"basher","initialPrompt":"go"}`
	}
	transcript := `[{"id":"ai-1","variant":"ai","timestamp":"03:04 PM","blocks":[` +
		agent("agent-1") + `,` + agent("a b/c") +
		`]}]`
	outcome, parentID := codebuffParseProviderDir(t, transcript)
	parent := codebuffResultByID(t, outcome, parentID)
	calls := codebuffAllToolCalls(parent.Messages)
	require.Len(t, calls, 2)

	prefix := parentID + "__subagent__"
	assert.Equal(t, prefix+"agent-1", calls[0].SubagentSessionID)
	unsafe := strings.TrimPrefix(calls[1].SubagentSessionID, prefix)
	assert.Regexp(t, `^h[0-9a-f]{16}$`, unsafe,
		"an agentId with unsafe characters is replaced by a digest")

	for _, c := range calls {
		child := codebuffResultByID(t, outcome, c.SubagentSessionID)
		assert.Equal(t, parentID, child.Session.ParentSessionID)
	}
}

func TestCodebuffSubagentWithoutWorkHasNoSession(t *testing.T) {
	transcript := `[{"id":"ai-1","variant":"ai","timestamp":"03:04 PM","blocks":[
		{"type":"agent","agentId":"a1","agentType":"basher","content":"done"}]}]`
	outcome, parentID := codebuffParseProviderDir(t, transcript)
	require.Len(t, outcome.Results, 1, "no prompt and no child blocks: nothing to store")
	calls := codebuffAllToolCalls(codebuffResultByID(t, outcome, parentID).Messages)
	require.Len(t, calls, 1)
	assert.Empty(t, calls[0].SubagentSessionID,
		"the call must not link to a session that does not exist")
}

func TestCodebuffFindFileResolvesSubagentIDs(t *testing.T) {
	root := t.TempDir()
	ts := "2026-07-15T20-01-32.065Z"
	chat := filepath.Join(root, "proj", "chats", ts, "chat-messages.json")
	codebuffWriteFile(t, chat, "[]")

	match, ok := codebuffFindFile(root, "proj:"+ts+"__subagent__agent-1")
	require.True(t, ok)
	assert.Equal(t, chat, match.Path)

	_, ok = codebuffFindFile(root, "proj:"+ts+"__subagent__")
	assert.False(t, ok, "an empty subagent key does not resolve")
	_, ok = codebuffFindFile(root, "proj:../x__subagent__a")
	assert.False(t, ok, "the timestamp before the key is still validated")
}
