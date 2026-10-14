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
	require.NoError(t, os.WriteFile(metadataPath, []byte(`{"schema_version":4,"title":"Before","created_at_ms":1735689601000,"updated_at_ms":1735689602000}`), 0o600))
	require.NoError(t, os.WriteFile(path, []byte(`{"schema_version":3,"seq":1,"timestamp_ms":1735689601000,"event":{"user":{"text":"Question"}}}
{"schema_version":3,"seq":2,"timestamp_ms":1735689602000,"event":{"assistant":{"text":"Answer"}}}
{"schema_version":3,"seq":3,"timestamp_ms":1735689602000,"event":{"tool_call":{"call_id":"read-1","tool_name":"read_file","arguments_json":"{}"}}}
{"schema_version":3,"seq":4,"timestamp_ms":1735689602000,"event":{"tool_result":{"call_id":"read-1","status":"success","preview":"File contents","artifact_ref":"result.txt","stored_bytes":21}}}
{"schema_version":3,"seq":5,"timestamp_ms":1735689603000,"event":{"turn_completed":{}}}
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
	messages, err := database.GetAllMessages(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, 2, session.MessageCount)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "read-1", messages[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "File contents", messages[1].ToolCalls[0].ResultContent)

	artifact := filepath.Join(dir, "tool-results", "result.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(artifact), 0o755))
	require.NoError(t, os.WriteFile(artifact, []byte("File contents in full"), 0o600))
	engine.SyncAll(t.Context(), nil)
	messages, err = database.GetAllMessages(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "File contents in full", messages[1].ToolCalls[0].ResultContent)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "usage-v2.json"), []byte(`{"snapshot":{"models":[{"model":"gpt-6.1-sol","input_tokens":20,"cache_read_tokens":5,"cache_write_tokens":null,"output_tokens":7,"reasoning_tokens":null,"request_count":null}]}}`), 0o600))
	engine.SyncAll(t.Context(), nil)
	usage, err := database.GetUsageEvents(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, usage, 1)

	logFile, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = logFile.WriteString(`{"schema_version":3,"seq":6,"timestamp_ms":1735689604000,"event":{"context_checkpoint":{"summary":"Compacted work"}}}` + "\n")
	require.NoError(t, logFile.Close())
	require.NoError(t, err)
	engine.SyncAll(t.Context(), nil)
	messages, err = database.GetAllMessages(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 3)
	assert.Equal(t, "Question", messages[0].Content)
	assert.Equal(t, "Answer", messages[1].Content)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "File contents in full", messages[1].ToolCalls[0].ResultContent)
	assert.Equal(t, "Compacted work", messages[2].Content)
	assert.True(t, messages[2].IsCompactBoundary)
}
