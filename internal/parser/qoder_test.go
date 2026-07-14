package parser

import (
	"context"
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
	assert.Equal(t, CapabilitySupported, caps.Source.MultiSessionSource)
	assert.Equal(t, CapabilitySupported, caps.Source.ExcludedSessions)
	assert.Equal(t, CapabilitySupported, caps.Source.ForceReplaceOnParse)
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
	content := fmt.Sprintf(`{"type":"agent-setting","agentSetting":"triage","entrypoint":"sdk-cli","sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"help me"},"cwd":%q,"sessionId":"11111111-1111-4111-8111-111111111111","version":"1.0.8"}
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
	assert.Equal(t, "", sess.AgentLabel)
	assert.Equal(t, "", sess.Entrypoint)
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

func TestQoderSidecarForkFromPreservesParserForkParent(t *testing.T) {
	t.Run("promotes continuation parent", func(t *testing.T) {
		sess := ParsedSession{
			ParentSessionID:  "qoder:22222222-2222-4222-8222-222222222222",
			RelationshipType: RelContinuation,
		}

		applyQoderMeta(&sess, qoderSessionMeta{
			ForkFrom: "11111111-1111-4111-8111-111111111111",
		})

		assert.Equal(t, "qoder:11111111-1111-4111-8111-111111111111", sess.ParentSessionID)
		assert.Equal(t, RelFork, sess.RelationshipType)
	})

	t.Run("keeps parser fork parent", func(t *testing.T) {
		sess := ParsedSession{
			ParentSessionID:  "qoder:22222222-2222-4222-8222-222222222222",
			RelationshipType: RelFork,
		}

		applyQoderMeta(&sess, qoderSessionMeta{
			ForkFrom: "11111111-1111-4111-8111-111111111111",
		})

		assert.Equal(t, "qoder:22222222-2222-4222-8222-222222222222", sess.ParentSessionID)
		assert.Equal(t, RelFork, sess.RelationshipType)
	})
}

func TestQoderProviderParseStampsCompositeFingerprint(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "-Users-alice-project", "11111111-1111-4111-8111-111111111111.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"hello"},"sessionId":"11111111-1111-4111-8111-111111111111"}
`), 0o644))
	require.NoError(t, os.WriteFile(
		strings.TrimSuffix(path, ".jsonl")+"-session.json",
		[]byte(`{"title":"Composite fingerprint"}`),
		0o644,
	))

	provider, ok := NewProvider(AgentQoder, ProviderConfig{
		Roots:   []string{root},
		Machine: "local",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	file := outcome.Results[0].Result.Session.File
	assert.Equal(t, fingerprint.Size, file.Size)
	assert.Equal(t, fingerprint.MTimeNS, file.Mtime)
	assert.Equal(t, fingerprint.Hash, file.Hash)
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

func TestParseQoderSubagentExclusionIDs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "proj", "11111111-1111-4111-8111-111111111111", "subagents", "agent-123.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"type":"user","uuid":"u1","timestamp":"2026-06-04T09:47:27.966Z","message":{"role":"user","content":"/usage"},"sessionId":"agent-123"}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	results, excluded, err := ParseQoderSessionWithExclusions(path, "proj", "local")
	require.NoError(t, err)
	assert.Empty(t, results)
	assert.Equal(t, []string{
		"qoder:11111111-1111-4111-8111-111111111111:subagent:agent-123",
	}, excluded)

	provider, ok := NewProvider(AgentQoder, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: "11111111-1111-4111-8111-111111111111:subagent:agent-123",
	})
	require.NoError(t, err)
	require.True(t, found)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:  source,
		Machine: "local",
	})
	require.NoError(t, err)
	assert.Empty(t, outcome.Results)
	assert.Equal(t, []string{
		"qoder:11111111-1111-4111-8111-111111111111:subagent:agent-123",
	}, outcome.ExcludedSessionIDs)
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

func TestParseQoderForkedSessionRetagsToolCallSubagentIDsToFileStem(t *testing.T) {
	fileStem := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(t.TempDir(), "proj", fileStem+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	content := `{"type":"user","uuid":"root","timestamp":"2026-06-04T09:47:20.000Z","message":{"role":"user","content":"start"},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"assistant","uuid":"split","parentUuid":"root","timestamp":"2026-06-04T09:47:21.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"user","uuid":"main-u1","parentUuid":"split","timestamp":"2026-06-04T09:47:22.000Z","message":{"role":"user","content":"main 1"},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"assistant","uuid":"main-a1","parentUuid":"main-u1","timestamp":"2026-06-04T09:47:23.000Z","message":{"role":"assistant","content":[{"type":"text","text":"main answer 1"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"user","uuid":"main-u2","parentUuid":"main-a1","timestamp":"2026-06-04T09:47:24.000Z","message":{"role":"user","content":"main 2"},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"assistant","uuid":"main-a2","parentUuid":"main-u2","timestamp":"2026-06-04T09:47:25.000Z","message":{"role":"assistant","content":[{"type":"text","text":"main answer 2"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"user","uuid":"main-u3","parentUuid":"main-a2","timestamp":"2026-06-04T09:47:26.000Z","message":{"role":"user","content":"main 3"},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"assistant","uuid":"main-a3","parentUuid":"main-u3","timestamp":"2026-06-04T09:47:27.000Z","message":{"role":"assistant","content":[{"type":"text","text":"main answer 3"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"user","uuid":"main-u4","parentUuid":"main-a3","timestamp":"2026-06-04T09:47:28.000Z","message":{"role":"user","content":"main 4"},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"assistant","uuid":"main-a4","parentUuid":"main-u4","timestamp":"2026-06-04T09:47:29.000Z","message":{"role":"assistant","content":[{"type":"text","text":"main answer 4"}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"user","uuid":"fork-u1","parentUuid":"split","timestamp":"2026-06-04T09:48:00.000Z","message":{"role":"user","content":"fork delegate"},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"assistant","uuid":"fork-a1","parentUuid":"fork-u1","timestamp":"2026-06-04T09:48:01.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_fork","name":"Agent","input":{"description":"do fork work","prompt":"x"}}]},"sessionId":"11111111-1111-4111-8111-111111111111"}
{"type":"user","uuid":"fork-r1","parentUuid":"fork-a1","timestamp":"2026-06-04T09:48:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_fork","content":"done"}]},"toolUseResult":{"status":"completed","agentId":"123"},"sessionId":"11111111-1111-4111-8111-111111111111"}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	require.NoError(t, os.WriteFile(
		strings.TrimSuffix(path, ".jsonl")+"-session.json",
		[]byte(`{"fork_from":"22222222-2222-4222-8222-222222222222"}`),
		0o644,
	))

	results, err := ParseQoderSession(path, "proj", "local")
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "qoder:22222222-2222-4222-8222-222222222222", results[0].Session.ParentSessionID)
	var fork *ParseResult
	for i := range results {
		if results[i].Session.RelationshipType == RelFork {
			fork = &results[i]
		}
	}
	require.NotNil(t, fork)
	assert.NotEqual(t, qoderPrefixID(fileStem), fork.Session.ID)
	assert.Equal(t, qoderPrefixID(fileStem), fork.Session.ParentSessionID)
	var calls []ParsedToolCall
	for _, msg := range fork.Messages {
		calls = append(calls, msg.ToolCalls...)
	}
	require.Len(t, calls, 1)
	assert.Equal(t, "qoder:"+fileStem+":subagent:agent-123", calls[0].SubagentSessionID)
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
		{"-Users-alice-code-sample-project", "sample_project"},
		{"-Users-alice-code-dev-tools", "dev_tools"},
		{"-home-alice-projects-work-app", "work_app"},
		{"-home-alice-projects-agent-ui", "agent_ui"},
		{"plain-name", "plain_name"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, DecodeQoderProjectDir(tt.name))
		})
	}
}
