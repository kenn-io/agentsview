package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestFxSyncCompanionChangesAndCompaction(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sync-session")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "events.jsonl")
	metadataPath := filepath.Join(dir, "session.json")
	require.NoError(t, os.WriteFile(metadataPath, []byte(`{"schema_version":4,"title":"Before","created_at_ms":1000,"updated_at_ms":2000}`), 0o600))
	require.NoError(t, os.WriteFile(path, []byte(`{"seq":1,"timestamp_ms":1000,"event":{"user":{"text":"Question"}}}
{"seq":2,"timestamp_ms":2000,"event":{"assistant":{"text":"Answer"}}}
{"seq":3,"timestamp_ms":2000,"event":{"tool_call":{"call_id":"read-1","tool_name":"read_file","arguments_json":"{}"}}}
{"seq":4,"timestamp_ms":2000,"event":{"tool_result":{"call_id":"read-1","status":"success","preview":"File contents"}}}
`), 0o600))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFx: {root}},
		Machine:   "local", DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)
	stats := engine.SyncAllForceParse(t.Context(), nil)
	assert.GreaterOrEqual(t, stats.Synced, 1)
	session, err := database.GetSessionFull(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.SessionName)
	assert.Equal(t, "Before", *session.SessionName)
	assert.Equal(t, 1, session.UserMessageCount)
	messages, err := database.GetAllMessages(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 3)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "read-1", messages[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "File contents", messages[1].ToolCalls[0].ResultContent)

	require.NoError(t, os.WriteFile(metadataPath, []byte(`{"schema_version":4,"title":"After rename","created_at_ms":1000,"updated_at_ms":2000}`), 0o600))
	engine.SyncAll(t.Context(), nil)
	session, err = database.GetSessionFull(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.SessionName)
	assert.Equal(t, "After rename", *session.SessionName)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "usage-v2.json"), []byte(`{"snapshot":{"models":[{"model":"gpt-6.1-sol","input_tokens":20,"cache_read_tokens":5,"output_tokens":7}]}}`), 0o600))
	engine.SyncAll(t.Context(), nil)
	session, err = database.GetSessionFull(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 7, session.TotalOutputTokens)
	assert.Equal(t, 20, session.PeakContextTokens)

	require.NoError(t, os.WriteFile(path, []byte(`{"seq":5,"timestamp_ms":3000,"event":{"context_checkpoint":{"summary":"Compacted work"}}}`+"\n"), 0o600))
	engine.SyncAll(t.Context(), nil)
	messages, err = database.GetAllMessages(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "Compacted work", messages[0].Content)
	assert.True(t, messages[0].IsCompactBoundary)
}
