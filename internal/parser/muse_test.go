package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	museFixtureParentID = "0199a1b2-6c3d-7e4f-8a5b-000000000001"
	museFixtureChildID  = "0199a1b2-6c3d-7e4f-8a5b-000000000002"
)

func museFixtureRoot() string {
	return filepath.Join("testdata", "muse", "sessions")
}

func museFixtureParentLog() string {
	return filepath.Join(museFixtureRoot(), "2026", "09", "10",
		museFixtureParentID, museLogName)
}

func museFixtureChildLog() string {
	return filepath.Join(museFixtureRoot(), "2026", "09", "10",
		museFixtureParentID, museSubagentDirName, museFixtureChildID, museLogName)
}

func TestParseMuseSessionFoldsRunEvents(t *testing.T) {
	result, err := parseMuseSession(t.Context(), museFixtureParentLog(), "host")
	require.NoError(t, err)
	require.NotNil(t, result)

	session := result.Session
	assert.Equal(t, "muse:"+museFixtureParentID, session.ID)
	assert.Equal(t, AgentMuse, session.Agent)
	assert.Equal(t, "host", session.Machine)
	assert.Equal(t, "/work/demo-app", session.Cwd)
	assert.Equal(t, "demo_app", session.Project)
	assert.Equal(t, "feature/login", session.GitBranch)
	assert.Equal(t, "1.4.2", session.SourceVersion)
	assert.Equal(t, "Login test fix", session.SessionName)
	assert.Equal(t, "Fix the failing login test", session.FirstMessage)
	assert.Equal(t, 1, session.UserMessageCount)
	assert.Equal(t, 4, session.MessageCount)
	assert.Equal(t, 1, session.MalformedLines, "partial last line")
	assert.Empty(t, session.ParentSessionID)
	assert.Equal(t, int64(1789000000000000), session.StartedAt.UnixMicro())
	assert.Equal(t, int64(1789000006000000), session.EndedAt.UnixMicro())
	assert.Equal(t, 120, session.TotalOutputTokens)
	assert.Equal(t, 1500, session.PeakContextTokens)

	messages := result.Messages
	require.Len(t, messages, 4)
	for i, message := range messages {
		assert.Equal(t, i, message.Ordinal)
	}

	user := messages[0]
	assert.Equal(t, RoleUser, user.Role)
	assert.Equal(t, "Fix the failing login test", user.Content)

	calls := messages[1]
	assert.Equal(t, RoleAssistant, calls.Role)
	assert.Equal(t, "resp-1", calls.SourceUUID)
	assert.True(t, calls.HasThinking)
	assert.Equal(t, "Read the test before changing code.", calls.ThinkingText)
	assert.Contains(t, calls.Content, "[Thinking]\nRead the test before changing code.\n[/Thinking]")
	assert.Contains(t, calls.Content, "$ go test ./login")
	assert.True(t, calls.HasToolUse)
	require.Len(t, calls.ToolCalls, 2)
	assert.Equal(t, "call-read", calls.ToolCalls[0].ToolUseID)
	assert.Equal(t, "read_file", calls.ToolCalls[0].ToolName)
	assert.Equal(t, "Read", calls.ToolCalls[0].Category)
	assert.JSONEq(t, `{"path":"login_test.go"}`, calls.ToolCalls[0].InputJSON)
	assert.Equal(t, "Bash", calls.ToolCalls[1].Category)
	assert.Equal(t, "muse-spark-1.3", calls.Model)
	// Usage logged before the tool calls still lands on their message.
	assert.JSONEq(t, `{"input_tokens":200,"output_tokens":80,"cache_read_input_tokens":1000,"cache_creation_input_tokens":0,"reasoning_tokens":30}`,
		string(calls.TokenUsage))
	assert.Equal(t, 80, calls.OutputTokens)
	assert.Equal(t, 1200, calls.ContextTokens)

	results := messages[2]
	assert.Equal(t, RoleUser, results.Role)
	assert.Empty(t, results.Content)
	require.Len(t, results.ToolResults, 2)
	assert.Equal(t, "call-read", results.ToolResults[0].ToolUseID)
	assert.Empty(t, results.ToolResults[0].Status)
	assert.Equal(t, "package login\n\nfunc TestLogin(t *testing.T) {}",
		DecodeContent(results.ToolResults[0].ContentRaw))
	assert.Equal(t, "call-bash", results.ToolResults[1].ToolUseID)
	assert.Equal(t, "errored", results.ToolResults[1].Status)

	reply := messages[3]
	assert.Equal(t, RoleAssistant, reply.Role)
	assert.Equal(t, "The test expected a trimmed username. I fixed the trim.", reply.Content)
	assert.False(t, reply.HasThinking)
	assert.False(t, reply.HasToolUse)
	assert.JSONEq(t, `{"input_tokens":200,"output_tokens":40,"cache_read_input_tokens":1300,"cache_creation_input_tokens":0}`,
		string(reply.TokenUsage))

	for _, message := range messages {
		assert.NotContains(t, message.Content, "mirrored child output")
	}

	// Every usage record follows its response's model_response_created,
	// as in real logs, so all usage lands on a reply.
	assert.Empty(t, result.UsageEvents)
}

func TestParseMuseSubagentLinksParentAndInheritsWorkspace(t *testing.T) {
	result, err := parseMuseSession(t.Context(), museFixtureChildLog(), "host")
	require.NoError(t, err)
	require.NotNil(t, result)

	session := result.Session
	assert.Equal(t, "muse:"+museFixtureChildID, session.ID)
	assert.Equal(t, "muse:"+museFixtureParentID, session.ParentSessionID)
	assert.Equal(t, RelSubagent, session.RelationshipType)
	assert.Equal(t, "/work/demo-app", session.Cwd)
	assert.Equal(t, "demo_app", session.Project)
	assert.Equal(t, "Summarize the login package.", session.FirstMessage)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, "login exposes Authenticate and TrimUsername.", result.Messages[1].Content)
	assert.Equal(t, 20, result.Messages[1].OutputTokens)
	assert.Empty(t, result.UsageEvents)
}

func TestParseMuseSessionIgnoresLogWithoutOwnStream(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026", "09", "10", "own-id")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, museLogName)
	require.NoError(t, os.WriteFile(path, []byte(
		`{"schema_version":1,"stream":{"kind":"session","id":"other-id"},"sequence":1,"recorded_at":1789000000000000,"payload_type":"runtime.session","payload":{"kind":"run","event":{"kind":"started","prompt":"not mine"}}}`+"\n",
	), 0o600))

	result, err := parseMuseSession(t.Context(), path, "host")
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestMuseToolCall(t *testing.T) {
	tests := []struct {
		name         string
		call         string
		wantInput    string
		wantCategory string
	}{
		{
			name:         "args encoded as a JSON string",
			call:         `{"args":"{\"query\":\"TrimUsername\"}","call_id":"c1","name":"search"}`,
			wantInput:    `{"query":"TrimUsername"}`,
			wantCategory: "Grep",
		},
		{
			name:         "args as an object",
			call:         `{"args":{"agent_path":"explore"},"call_id":"c1","name":"subagent_spawn"}`,
			wantInput:    `{"agent_path":"explore"}`,
			wantCategory: "Task",
		},
		{
			name:         "args string that is not JSON",
			call:         `{"args":"raw text","call_id":"c1","name":"web_fetch"}`,
			wantInput:    `"raw text"`,
			wantCategory: "Tool",
		},
		{
			name:         "missing args",
			call:         `{"call_id":"c1","name":"write_todos"}`,
			wantInput:    `{}`,
			wantCategory: NormalizeToolCategory("write_todos"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, rendered := museToolCall(gjson.Parse(tt.call))
			assert.Equal(t, "c1", parsed.ToolUseID)
			assert.JSONEq(t, tt.wantInput, parsed.InputJSON)
			assert.Equal(t, tt.wantCategory, parsed.Category)
			assert.NotEmpty(t, rendered)
		})
	}
}
