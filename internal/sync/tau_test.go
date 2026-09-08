package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func tauSyncTranscript(leaf string) string {
	lines := []string{
		`{"id":"info","type":"session_info","timestamp":1700000000,"created_at":1700000000,"cwd":"/tmp/project"}`,
		`{"id":"u1","parent_id":"info","type":"message","message":{"role":"user","content":"first"}}`,
		`{"id":"a1","parent_id":"u1","type":"message","message":{"role":"assistant","content":[{"type":"text","text":"answer"},{"type":"toolCall","id":"tool-1","name":"search","arguments":{"path":"a.txt"}}],"usage":{"input":10,"output":5}}}`,
		`{"id":"tr1","parent_id":"a1","type":"message","message":{"role":"toolResult","toolCallId":"tool-1","toolName":"search","content":[{"type":"text","text":"done"}]}}`,
	}
	if leaf == "a2" {
		lines = append(lines,
			`{"id":"u2","parent_id":"u1","type":"message","message":{"role":"user","content":"second"}}`,
			`{"id":"a2","parent_id":"u2","type":"message","message":{"role":"assistant","content":"new answer","usage":{"input":2,"output":3}}}`,
		)
	}
	if leaf == "empty" {
		lines = append(lines, `{"id":"leaf","type":"leaf","entry_id":null}`)
	} else {
		lines = append(lines, `{"id":"leaf","type":"leaf","entry_id":"`+leaf+`"}`)
	}
	return tauTestLines(lines...)
}

func tauTestLines(lines ...string) string {
	result := ""
	for _, line := range lines {
		result += line + "\n"
	}
	return result
}

func TestTauSyncReplacesSelectedHistoryAndRetainsParseErrors(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	path := filepath.Join(project, "sync-session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(tauSyncTranscript("tr1")), 0o644))
	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentTau: {root}},
		Machine:   "local", DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)

	stats := engine.SyncAllForceParse(context.Background(), nil)
	assert.GreaterOrEqual(t, stats.Synced, 1)
	session, err := database.GetSessionFull(t.Context(), "tau:sync-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 2, session.MessageCount)
	assert.Equal(t, 5, session.TotalOutputTokens)
	assert.Equal(t, 10, session.PeakContextTokens)
	messages, err := database.GetAllMessages(t.Context(), "tau:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "tool-1", messages[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "done", messages[1].ToolCalls[0].ResultContent)

	require.NoError(t, os.WriteFile(path, []byte(tauSyncTranscript("a2")), 0o644))
	stats = engine.SyncAllForceParse(context.Background(), nil)
	assert.GreaterOrEqual(t, stats.Synced, 1)
	session, err = database.GetSessionFull(t.Context(), "tau:sync-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 3, session.MessageCount)
	assert.Equal(t, 3, session.TotalOutputTokens)
	messages, err = database.GetAllMessages(t.Context(), "tau:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 3)
	assert.Equal(t, "a2", messages[2].SourceUUID)

	beforeMessages := append([]string(nil), messages[0].Content, messages[1].Content, messages[2].Content)
	require.NoError(t, os.WriteFile(path, []byte("{malformed\n"), 0o644))
	engine.SyncAllForceParse(context.Background(), nil)
	messages, err = database.GetAllMessages(t.Context(), "tau:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 3)
	assert.Equal(t, beforeMessages, []string{messages[0].Content, messages[1].Content, messages[2].Content})

	require.NoError(t, os.WriteFile(path, []byte(tauSyncTranscript("empty")), 0o644))
	stats = engine.SyncAllForceParse(context.Background(), nil)
	assert.GreaterOrEqual(t, stats.Synced, 1)
	messages, err = database.GetAllMessages(t.Context(), "tau:sync-session")
	require.NoError(t, err)
	assert.Empty(t, messages)
	session, err = database.GetSessionFull(t.Context(), "tau:sync-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 0, session.MessageCount)
}
