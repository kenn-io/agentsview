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
	require.NoError(t, os.WriteFile(path, []byte(`{"seq":1,"timestamp_ms":1735689601000,"event":{"user":{"text":"Question"}}}
{"seq":2,"timestamp_ms":1735689602000,"event":{"assistant":{"text":"Answer"}}}
{"seq":3,"timestamp_ms":1735689602000,"event":{"tool_call":{"call_id":"read-1","tool_name":"read_file","arguments_json":"{}"}}}
{"seq":4,"timestamp_ms":1735689602000,"event":{"tool_result":{"call_id":"read-1","status":"success","preview":"File contents"}}}
{"seq":5,"timestamp_ms":1735689603000,"event":{"turn_completed":{}}}
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
	require.Len(t, messages, 2)
	assert.Equal(t, 2, session.MessageCount)
	require.Len(t, messages[1].ToolCalls, 1)
	assert.Equal(t, "read-1", messages[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "File contents", messages[1].ToolCalls[0].ResultContent)
	require.Len(t, messages[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "completed", messages[1].ToolCalls[0].ResultEvents[0].Status)
	assert.Equal(t, "tool_result", messages[1].ToolCalls[0].ResultEvents[0].Source)
	assert.Equal(t, "File contents", messages[1].ToolCalls[0].ResultEvents[0].Content)

	require.NoError(t, os.WriteFile(metadataPath, []byte(`{"schema_version":4,"title":"After rename","created_at_ms":1735689601000,"updated_at_ms":1735689602000}`), 0o600))
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
	assert.Zero(t, session.PeakContextTokens)
	assert.False(t, session.HasPeakContextTokens)
	usage, err := database.GetUsageEvents(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, "2025-01-01T00:00:03Z", usage[0].OccurredAt)

	require.NoError(t, os.WriteFile(metadataPath, []byte(`{"schema_version":4,"title":"After preferences","created_at_ms":1735689601000,"updated_at_ms":1735776000000}`), 0o600))
	engine.SyncAll(t.Context(), nil)
	usage, err = database.GetUsageEvents(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, "2025-01-01T00:00:03Z", usage[0].OccurredAt)
	assert.Equal(t, 15, usage[0].InputTokens)
	assert.Equal(t, 5, usage[0].CacheReadInputTokens)
	assert.Equal(t, 7, usage[0].OutputTokens)

	transcript, err := os.ReadFile(path)
	require.NoError(t, err)
	usagePath := filepath.Join(dir, "usage-v2.json")
	usageData, err := os.ReadFile(usagePath)
	require.NoError(t, err)
	for _, test := range []struct{ name, file, content string }{
		{"broken frame", path, string(transcript) + "{broken\n"},
		{"null usage", usagePath, `null`},
		{"missing snapshot", usagePath, `{}`},
		{"wrong session", usagePath, `{"session_id":"other-session","snapshot":{"models":[]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(test.file, []byte(test.content), 0o600))
			engine.SyncAll(t.Context(), nil)
			stored, err := database.GetAllMessages(t.Context(), "fx:sync-session")
			require.NoError(t, err)
			require.Len(t, stored, 2)
			assert.Equal(t, "Answer", stored[1].Content)
			storedUsage, err := database.GetUsageEvents(t.Context(), "fx:sync-session")
			require.NoError(t, err)
			require.Len(t, storedUsage, 1)
			assert.Equal(t, 7, storedUsage[0].OutputTokens)
			require.NoError(t, os.WriteFile(path, transcript, 0o600))
			require.NoError(t, os.WriteFile(usagePath, usageData, 0o600))
		})
	}

	require.NoError(t, os.WriteFile(path, []byte(`{"seq":6,"timestamp_ms":1735689604000,"event":{"context_checkpoint":{"summary":"Compacted work"}}}`+"\n"), 0o600))
	engine.SyncAll(t.Context(), nil)
	messages, err = database.GetAllMessages(t.Context(), "fx:sync-session")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "Compacted work", messages[0].Content)
	assert.True(t, messages[0].IsCompactBoundary)
}

func TestFxSyncChildBeforeParent(t *testing.T) {
	root := t.TempDir()
	childDir := filepath.Join(root, "a-child")
	require.NoError(t, os.MkdirAll(filepath.Join(childDir, "subagent"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "session.json"), []byte(`{"schema_version":4,"created_at_ms":1000,"updated_at_ms":2000}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "events.jsonl"), []byte(`{"schema_version":3,"timestamp_ms":2000,"event":{"user":{"text":"Child task"}}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "subagent", "owner.json"), []byte(`{"schema_version":1,"parent_id":"z-parent"}`), 0o600))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFx: {root}},
		Machine:   "local", DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)
	engine.SyncAllForceParse(t.Context(), nil)
	child, err := database.GetSessionFull(t.Context(), "fx:a-child")
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "fx:z-parent", *child.ParentSessionID)
	assert.Equal(t, string(parser.RelSubagent), child.RelationshipType)

	parentDir := filepath.Join(root, "z-parent")
	require.NoError(t, os.MkdirAll(parentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(parentDir, "session.json"), []byte(`{"schema_version":4,"created_at_ms":1000,"updated_at_ms":2000}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(parentDir, "events.jsonl"), []byte(`{"schema_version":3,"timestamp_ms":1000,"event":{"user":{"text":"Parent task"}}}`), 0o600))
	engine.SyncAllForceParse(t.Context(), nil)
	parent, err := database.GetSessionFull(t.Context(), "fx:z-parent")
	require.NoError(t, err)
	require.NotNil(t, parent)
	child, err = database.GetSessionFull(t.Context(), "fx:a-child")
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, parent.ID, *child.ParentSessionID)
	assert.Equal(t, string(parser.RelSubagent), child.RelationshipType)
	require.NotNil(t, child.FirstMessage)
	assert.Equal(t, "Child task", *child.FirstMessage)
}

func TestFxSyncFixtureToolResults(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "parent-session")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, name := range []string{"session.json", "events.jsonl", "usage-v2.json"} {
		data, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "fx", "parent-session", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFx: {root}},
		Machine:   "local", DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)
	engine.SyncAllForceParse(t.Context(), nil)
	messages, err := database.GetAllMessages(t.Context(), "fx:parent-session")
	require.NoError(t, err)
	require.Len(t, messages, 11)
	var statuses []string
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			require.Len(t, call.ResultEvents, 1)
			statuses = append(statuses, call.ResultEvents[0].Status)
			assert.Equal(t, "tool_result", call.ResultEvents[0].Source)
		}
	}
	assert.Equal(t, []string{"completed", "completed", "errored", "completed", "completed", "completed", "completed", "completed", "completed", "errored"}, statuses)
	usage, err := database.GetUsageEvents(t.Context(), "fx:parent-session")
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, "gpt-6.1-sol", usage[0].Model)
	assert.Equal(t, "codex", usage[0].ProviderID)
	assert.Equal(t, usage[0].Model, messages[1].Model)
	assert.Equal(t, usage[0].ProviderID, messages[1].ProviderID)
}
