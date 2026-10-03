package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClawNativeRecordIdentities(t *testing.T) {
	records := []string{
		`{"type":"session","id":"native","cwd":"/workspace/project"}`,
		`{"type":"message","id":"user-native","message":{"role":"user","content":[{"type":"text","text":"literal"}]}}`,
		`{"type":"message","id":"assistant-native","parentId":"user-native","message":{"role":"assistant","content":[{"type":"thinking","thinking":"plan"}]}}`,
		`{"type":"message","id":"result-native","parentId":"assistant-native","message":{"role":"toolResult","toolCallId":"","content":[{"type":"text","text":"output-needle"}]}}`,
	}
	for _, agent := range []AgentType{AgentOpenClaw, AgentQClaw} {
		t.Run(string(agent), func(t *testing.T) {
			var msgs []ParsedMessage
			var err error
			if agent == AgentOpenClaw {
				path, _ := writeOpenClawTestFile(t, "main", records...)
				_, msgs, err = parseOpenClawSessionForTest(t, path, "project", "local")
			} else {
				path, _ := writeQClawTestFile(t, "main", records...)
				_, msgs, err = parseQClawSessionForTest(t, path, "project", "local")
			}
			require.NoError(t, err)
			require.Len(t, msgs, 3)
			assert.Equal(t, "user-native", msgs[0].SourceUUID)
			assert.Equal(t, "assistant-native", msgs[1].SourceUUID)
			assert.Equal(t, "user-native", msgs[1].SourceParentUUID)
			assert.Equal(t, "result-native", msgs[2].SourceUUID)
			assert.Equal(t, "assistant-native", msgs[2].SourceParentUUID)
			assert.Equal(t, "output-needle", msgs[2].ToolResultText)
		})
	}
}

func TestGooseNativeRecordIdentities(t *testing.T) {
	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "native", "native", "user", "")
	fixture.insertMessage(t, "native", "user", `[{"type":"text","text":"literal"}]`, 1700000000)
	fixture.insertMessage(t, "native", "assistant", `[{"type":"thinking","thinking":"plan"}]`, 1700000001)
	provider, ok := NewProvider(AgentGoose, ProviderConfig{Roots: []string{fixture.pathRoot}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, "native:1", msgs[0].SourceUUID)
	assert.Equal(t, "native:2", msgs[1].SourceUUID)
}

func TestZCodeNativeRecordIdentities(t *testing.T) {
	fixture := newZCodeTestFixture(t)
	fixture.insertSession(t, "native", "/workspace/project", "native", "2026-10-01T00:00:00Z", "2026-10-01T00:01:00Z", "", "")
	fixture.insertMessage(t, "user-native", "native", "2026-10-01T00:00:01Z", `{"role":"user"}`)
	fixture.insertPart(t, "user-part", "user-native", "native", `{"type":"text","text":"literal"}`)
	fixture.insertMessage(t, "assistant-native", "native", "2026-10-01T00:00:02Z", `{"role":"assistant"}`)
	fixture.insertPart(t, "assistant-part", "assistant-native", "native", `{"type":"thinking","thinking":"plan"}`)
	result, err := parseZCodeSession(t.Context(), fixture.DBPath, "native", "local", false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Messages, 2)
	assert.Equal(t, "user-native", result.Messages[0].SourceUUID)
	assert.Equal(t, "assistant-native", result.Messages[1].SourceUUID)
}

func TestCodebuffNativeSingleRecordIdentities(t *testing.T) {
	dir := codebuffTestSession(t, `[
{"id":"user-native","variant":"user","content":"literal","timestamp":"2026-10-01T00:00:00Z"},
{"id":"answer-native","parentId":"user-native","variant":"ai","timestamp":"2026-10-01T00:00:01Z","blocks":[{"type":"text","content":"answer"}]},
{"id":"split-native","parentId":"answer-native","variant":"ai","timestamp":"2026-10-01T00:00:02Z","blocks":[{"type":"text","textType":"reasoning","content":"plan"},{"type":"text","content":"after"}]},
{"id":"error-native","variant":"error","content":"notice","timestamp":"2026-10-01T00:00:03Z"}]`, "", "")
	_, msgs, err := parseCodebuffSession(dir, "project", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 5)
	assert.Equal(t, "user-native", msgs[0].SourceUUID)
	assert.Equal(t, "answer-native", msgs[1].SourceUUID)
	assert.Equal(t, "user-native", msgs[1].SourceParentUUID)
	assert.Empty(t, msgs[2].SourceUUID)
	assert.Empty(t, msgs[3].SourceUUID)
	assert.Equal(t, "error-native", msgs[4].SourceUUID)
	assert.True(t, msgs[4].IsSystem)
}

func TestQwenNativeUnambiguousAssistantIdentity(t *testing.T) {
	cases := []struct{ name, prefix, wantID string }{
		{name: "single record", wantID: "answer-native"},
		{name: "same native identity", prefix: `{"type":"assistant","uuid":"answer-native","parentUuid":"user-native","message":{"role":"model","parts":[{"thought":true,"text":"plan"}]}}`, wantID: "answer-native"},
		{name: "distinct records", prefix: `{"type":"assistant","uuid":"first-native","parentUuid":"user-native","message":{"role":"model","parts":[{"thought":true,"text":"plan"}]}}`},
		{name: "missing identity", prefix: `{"type":"assistant","message":{"role":"model","parts":[{"thought":true,"text":"plan"}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := createTestFile(t, "native.jsonl", tc.prefix+"\n"+`{"type":"assistant","uuid":"answer-native","parentUuid":"user-native","message":{"role":"model","parts":[{"text":"answer"}]}}`)
			_, msgs, err := parseQwenSession(path, "project", "local")
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			assert.Equal(t, "answer", msgs[0].Content)
			assert.Equal(t, tc.wantID, msgs[0].SourceUUID)
			if tc.wantID != "" {
				assert.Equal(t, "user-native", msgs[0].SourceParentUUID)
			}
		})
	}
}
