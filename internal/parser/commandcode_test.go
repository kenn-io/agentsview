package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommandCodeProviderDiscoversSessions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	root := t.TempDir()
	projectDir := filepath.Join(root, "users-alice-code-sample-project")
	require.NoError(os.MkdirAll(projectDir, 0o755))
	require.NoError(os.WriteFile(filepath.Join(projectDir, "sess_a.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(projectDir, "sess_a.meta.json"), []byte("{}"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(projectDir, "sess_a.checkpoints.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(projectDir, "sess_a.prompts.jsonl"), []byte("{}\n"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(projectDir, "notes.txt"), []byte("ignore"), 0o644))

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal(AgentCommandCode, sources[0].Provider)
	assert.Equal(filepath.Join(projectDir, "sess_a.jsonl"), sources[0].DisplayPath)
}

func TestCommandCodeProviderFindsSourceFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	root := t.TempDir()
	projectDir := filepath.Join(root, "users-alice-code-sample-project")
	require.NoError(os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "sess_123.jsonl")
	require.NoError(os.WriteFile(path, []byte("{}\n"), 0o644))

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{Roots: []string{root}})
	require.True(ok)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "sess_123",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(path, found.DisplayPath)

	_, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "sess_missing",
	})
	require.NoError(err)
	assert.False(ok)
}

func TestCommandCodeProviderParsesSession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	content := `{"id":"m1","timestamp":"2026-06-01T10:00:00Z","sessionId":"sess_123","role":"user","content":[{"type":"text","text":"Inspect server logs"}],"gitBranch":"feature/command-code","metadata":{"version":2,"cwd":"/Users/alice/code/sample-project"}}
{"id":"m2","timestamp":"2026-06-01T10:00:01Z","sessionId":"sess_123","role":"assistant","content":[{"type":"reasoning","text":"I should read the logs first."},{"type":"tool-call","toolCallId":"tc1","toolName":"Read","input":{"file_path":"server.log"}}],"gitBranch":"feature/command-code","metadata":{"version":2}}
{"id":"m3","timestamp":"2026-06-01T10:00:02Z","sessionId":"sess_123","role":"tool","content":[{"type":"tool-result","toolCallId":"tc1","toolName":"Read","output":{"type":"text","value":"error: boom"}}],"gitBranch":"feature/command-code","metadata":{"version":2}}
{"id":"m4","timestamp":"2026-06-01T10:00:03Z","sessionId":"sess_123","role":"assistant","content":[{"type":"text","text":"The error is in the startup path."}],"gitBranch":"feature/command-code","metadata":{"version":2}}`

	root := t.TempDir()
	path := filepath.Join(root, "project", "sess_123.jsonl")
	writeSourceFile(t, path, content)
	metaPath := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
	require.NoError(os.WriteFile(metaPath, []byte(`{"title":"Startup investigation"}`), 0o644))

	provider, ok := NewProvider(AgentCommandCode, ProviderConfig{
		Roots:   []string{root},
		Machine: "local",
	})
	require.True(ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: "sess_123",
	})
	require.NoError(err)
	require.True(found)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:  source,
		Machine: "local",
	})
	require.NoError(err)
	require.Len(outcome.Results, 1)

	sess := outcome.Results[0].Result.Session
	msgs := outcome.Results[0].Result.Messages
	require.Len(msgs, 4)

	assert.Equal("commandcode:sess_123", sess.ID)
	assert.Equal(AgentCommandCode, sess.Agent)
	assert.Equal("sample_project", sess.Project)
	assert.Equal("/Users/alice/code/sample-project", sess.Cwd)
	assert.Equal("feature/command-code", sess.GitBranch)
	assert.Equal("Inspect server logs", sess.FirstMessage)
	assert.Equal("Startup investigation", sess.SessionName)
	assert.Equal(4, sess.MessageCount)
	assert.Equal(1, sess.UserMessageCount)

	assert.Equal(RoleUser, msgs[0].Role)
	assert.Equal("Inspect server logs", msgs[0].Content)

	assert.Equal(RoleAssistant, msgs[1].Role)
	assert.True(msgs[1].HasThinking)
	assert.Equal("I should read the logs first.", msgs[1].ThinkingText)
	require.Len(msgs[1].ToolCalls, 1)
	assert.Equal("tc1", msgs[1].ToolCalls[0].ToolUseID)
	assert.Equal("Read", msgs[1].ToolCalls[0].ToolName)

	assert.Equal(RoleUser, msgs[2].Role)
	require.Len(msgs[2].ToolResults, 1)
	assert.Equal("tc1", msgs[2].ToolResults[0].ToolUseID)
	assert.Equal("error: boom", DecodeContent(msgs[2].ToolResults[0].ContentRaw))

	assert.Equal(RoleAssistant, msgs[3].Role)
	assert.Equal("The error is in the startup path.", msgs[3].Content)
}
