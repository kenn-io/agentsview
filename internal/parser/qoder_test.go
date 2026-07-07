package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQoderRegistry(t *testing.T) {
	def, ok := AgentByType(AgentQoder)
	require.True(t, ok, "AgentQoder missing from Registry")
	assert.Equal(t, "Qoder", def.DisplayName)
	assert.Equal(t, "QODER_PROJECTS_DIR", def.EnvVar)
	assert.Equal(t, "qoder_project_dirs", def.ConfigKey)
	assert.Equal(t, qoderIDPrefix, def.IDPrefix)
	assert.True(t, def.FileBased)
	assert.False(t, def.ShallowWatch)
	assert.Equal(t, []string{".qoder/projects", ".qoderwork/projects"}, def.DefaultDirs)
	factory, ok := ProviderFactoryByType(AgentQoder)
	require.True(t, ok, "AgentQoder provider missing")
	assert.Equal(t, AgentQoder, factory.Definition().Type)
	caps := factory.Capabilities()
	assert.Equal(t, CapabilitySupported, caps.Source.DiscoverSources)
	assert.Equal(t, CapabilitySupported, caps.Source.ClassifyChangedPath)
	assert.Equal(t, CapabilitySupported, caps.Source.FindSource)
	assert.Equal(t, CapabilitySupported, caps.Content.Subagents)
	provider, ok := NewProvider(AgentQoder, ProviderConfig{Roots: []string{t.TempDir()}})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestParseQoderSession(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "code", "sample-project")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	path := filepath.Join(root, "-Users-alice-sample-project", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := fmt.Sprintf(`{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"help me"},"cwd":%q,"sessionId":"11111111-1111-4111-8111-111111111111","version":"1.0.8"}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-06-04T09:47:32.116Z","message":{"id":"msg_1","type":"message","role":"assistant","model":"auto","stop_reason":"end_turn","usage":{"input_tokens":100,"cache_creation_input_tokens":7,"cache_read_input_tokens":50,"output_tokens":30},"content":[{"type":"text","text":"done"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2026-06-04T09:47:39.020Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
`, cwd)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	results, err := ParseQoderSession(path, "sample-project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	sess := results[0].Session
	assert.Equal(t, "qoder:11111111-1111-4111-8111-111111111111", sess.ID)
	assert.Equal(t, AgentQoder, sess.Agent)
	assert.Equal(t, "sample-project", sess.Project)
	assert.Equal(t, cwd, sess.Cwd)
	assert.Equal(t, "help me", sess.FirstMessage)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 30, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 157, sess.PeakContextTokens)
	require.Len(t, results[0].Messages, 3)
	assert.Equal(t, 157, results[0].Messages[1].ContextTokens)
	assert.Equal(t, 30, results[0].Messages[1].OutputTokens)
}

func TestParseQoderSessionSidecarMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "22222222-2222-4222-8222-222222222222.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"fork"}}
`), 0o644))
	require.NoError(t, os.WriteFile(
		strings.TrimSuffix(path, ".jsonl")+"-session.json",
		[]byte(`{"title":"Forked work","working_dir":"/tmp/qoder","fork_from":"11111111-1111-4111-8111-111111111111"}`),
		0o644,
	))

	results, err := ParseQoderSession(path, "proj", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	sess := results[0].Session
	assert.Equal(t, "Forked work", sess.SessionName)
	assert.Equal(t, "/tmp/qoder", sess.Cwd)
	assert.Equal(t, "qoder:11111111-1111-4111-8111-111111111111", sess.ParentSessionID)
	assert.Equal(t, RelFork, sess.RelationshipType)
}

func TestParseQoderSubagentSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"agentId":"123","type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":[{"type":"text","text":"sub task"}]},"sessionId":"child-session"}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	results, err := ParseQoderSession(path, "proj", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	sess := results[0].Session
	assert.Equal(t, "qoder:11111111-1111-4111-8111-111111111111:subagent:agent-123", sess.ID)
	assert.Equal(t, "qoder:11111111-1111-4111-8111-111111111111", sess.ParentSessionID)
	assert.Equal(t, RelSubagent, sess.RelationshipType)
	assert.Equal(t, AgentQoder, sess.Agent)
}

func TestParseQoderSessionRetagsToolCallSubagentIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"delegate"},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-06-04T09:47:32.116Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Agent","input":{"description":"do it","prompt":"x"}}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2026-06-04T09:47:39.020Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]},"toolUseResult":{"status":"completed","agentId":"123"}}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	results, err := ParseQoderSession(path, "proj", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	var calls []ParsedToolCall
	for _, msg := range results[0].Messages {
		calls = append(calls, msg.ToolCalls...)
	}
	require.Len(t, calls, 1)
	assert.Equal(t, "qoder:11111111-1111-4111-8111-111111111111:subagent:agent-123", calls[0].SubagentSessionID)
}

func TestDiscoverQoderSessions(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "-Users-alice-project", "11111111-1111-4111-8111-111111111111.jsonl")
	sidecarPath := filepath.Join(root, "-Users-alice-project", "11111111-1111-4111-8111-111111111111-session.json")
	statsPath := filepath.Join(root, "ai-stats", "usage.json")
	rootAgentPath := filepath.Join(root, "-Users-alice-project", "agent-123.jsonl")
	subPath := filepath.Join(root, "-Users-alice-project", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	for _, path := range []string{mainPath, sidecarPath, statsPath, rootAgentPath, subPath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	}

	files := DiscoverQoderSessions(root)
	require.Len(t, files, 2)
	assert.Equal(t, mainPath, files[0].Path)
	assert.Equal(t, "project", files[0].Project)
	assert.Equal(t, AgentQoder, files[0].Agent)
	assert.Equal(t, subPath, files[1].Path)
	assert.Equal(t, "project", files[1].Project)
	assert.Equal(t, AgentQoder, files[1].Agent)
}

func TestFindQoderSourceFile(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111.jsonl")
	subPath := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	for _, path := range []string{mainPath, subPath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	}

	assert.Equal(t, mainPath, FindQoderSourceFile(root, "qoder:11111111-1111-4111-8111-111111111111"))
	assert.Equal(t, subPath, FindQoderSourceFile(root, "qoder:11111111-1111-4111-8111-111111111111:subagent:agent-123"))
	assert.Empty(t, FindQoderSourceFile(root, "qoder:11111111-1111-4111-8111-111111111111:subagent:../agent-123"))
}

func TestDecodeQoderProjectDir(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"-Users-foo-myproject", "myproject"},
		{"-home-user-work-coding", "coding"},
		{"plain-name", "plain_name"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, DecodeQoderProjectDir(tt.name))
		})
	}
}
