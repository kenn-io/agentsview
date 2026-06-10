package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverCommandCodeSessions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	projectDir := filepath.Join(root, "users-alice-code-sample-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "sess_a.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "sess_a.meta.json"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "sess_a.checkpoints.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "sess_a.prompts.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "notes.txt"), []byte("ignore"), 0o644))

	files := DiscoverCommandCodeSessions(root)
	require.Len(t, files, 1)
	assert.Equal(t, AgentCommandCode, files[0].Agent)
	assert.Equal(t, filepath.Join(projectDir, "sess_a.jsonl"), files[0].Path)
}

func TestFindCommandCodeSourceFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	projectDir := filepath.Join(root, "users-alice-code-sample-project")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "sess_123.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))

	assert.Equal(t, path, FindCommandCodeSourceFile(root, "sess_123"))
	assert.Empty(t, FindCommandCodeSourceFile(root, "sess_missing"))
}

func TestParseCommandCodeSession(t *testing.T) {
	t.Parallel()

	content := `{"id":"m1","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess_123","role":"user","content":[{"type":"text","text":"Inspect server logs"}],"gitBranch":"feature/command-code","metadata":{"version":2,"cwd":"/Users/alice/code/sample-project"}}
{"id":"m2","timestamp":"2026-06-01T10:00:01Z","sessionId":"sess_123","role":"assistant","content":[{"type":"reasoning","text":"I should read the logs first."},{"type":"tool-call","toolCallId":"tc1","toolName":"Read","input":{"file_path":"server.log"}}],"gitBranch":"feature/command-code","metadata":{"version":2}}
{"id":"m3","timestamp":"2026-06-01T10:00:02Z","sessionId":"sess_123","role":"tool","content":[{"type":"tool-result","toolCallId":"tc1","toolName":"Read","output":{"type":"text","value":"error: boom"}}],"gitBranch":"feature/command-code","metadata":{"version":2}}
{"id":"m4","timestamp":"2026-06-01T10:00:03Z","sessionId":"sess_123","role":"assistant","content":[{"type":"text","text":"The error is in the startup path."}],"gitBranch":"feature/command-code","metadata":{"version":2}}`

	path := createTestFile(t, "commandcode.jsonl", content)
	metaPath := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"title":"Startup investigation"}`), 0o644))

	sess, msgs, err := ParseCommandCodeSession(path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 4)

	assert.Equal(t, "commandcode:sess_123", sess.ID)
	assert.Equal(t, AgentCommandCode, sess.Agent)
	assert.Equal(t, "sample_project", sess.Project)
	assert.Equal(t, "/Users/alice/code/sample-project", sess.Cwd)
	assert.Equal(t, "feature/command-code", sess.GitBranch)
	assert.Equal(t, "Inspect server logs", sess.FirstMessage)
	assert.Equal(t, "Startup investigation", sess.SessionName)
	assert.Equal(t, 4, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Inspect server logs", msgs[0].Content)

	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasThinking)
	assert.Equal(t, "I should read the logs first.", msgs[1].ThinkingText)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "tc1", msgs[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].ToolName)

	assert.Equal(t, RoleUser, msgs[2].Role)
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "tc1", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal(t, "error: boom", DecodeContent(msgs[2].ToolResults[0].ContentRaw))

	assert.Equal(t, RoleAssistant, msgs[3].Role)
	assert.Equal(t, "The error is in the startup path.", msgs[3].Content)
}
