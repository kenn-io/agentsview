package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
{"seq":4,"timestamp_ms":1735689602000,"event":{"tool_result":{"call_id":"read-1","status":"success","preview":"File contents","artifact_ref":"result.txt","stored_bytes":21}}}
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

	artifact := filepath.Join(dir, "tool-results", "result.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(artifact), 0o755))
	require.NoError(t, os.WriteFile(artifact, []byte("File contents in full"), 0o600))
	provider, ok := parser.NewProvider(parser.AgentFx, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	changed, err := provider.SourcesForChangedPath(t.Context(), parser.ChangedPathRequest{Path: artifact, EventKind: "write", WatchRoot: root})
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, path, changed[0].DisplayPath)
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
	assert.Equal(t, "2025-01-01T00:00:03Z", usage[0].OccurredAt)

	transcript, err := os.ReadFile(path)
	require.NoError(t, err)
	t.Run("broken frame", func(t *testing.T) {
		require.NoError(t, os.WriteFile(path, append(transcript, []byte("{broken\n")...), 0o600))
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
	})

	logFile, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = logFile.WriteString(`{"seq":6,"timestamp_ms":1735689604000,"event":{"context_checkpoint":{"summary":"Compacted work"}}}` + "\n")
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

func TestFxSyncChildBeforeParent(t *testing.T) {
	root := t.TempDir()
	childDir := filepath.Join(root, "a.child")
	require.NoError(t, os.MkdirAll(filepath.Join(childDir, "subagent"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "session.json"), []byte(`{"schema_version":4,"created_at_ms":1000,"updated_at_ms":2000}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "events.jsonl"), []byte(`{"schema_version":3,"timestamp_ms":2000,"event":{"user":{"text":"Child task"}}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(childDir, "subagent", "owner.json"), []byte(`{"schema_version":1,"parent_id":"z.parent"}`), 0o600))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentFx: {root}},
		Machine:   "local", DisableSignalRecomputation: true,
	})
	t.Cleanup(engine.Close)
	engine.SyncAllForceParse(t.Context(), nil)
	child, err := database.GetSessionFull(t.Context(), "fx:a.child")
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "fx:z.parent", *child.ParentSessionID)
	assert.Equal(t, string(parser.RelSubagent), child.RelationshipType)

	parentDir := filepath.Join(root, "z.parent")
	require.NoError(t, os.MkdirAll(parentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(parentDir, "session.json"), []byte(`{"schema_version":4,"created_at_ms":1000,"updated_at_ms":2000}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(parentDir, "events.jsonl"), []byte(`{"schema_version":3,"timestamp_ms":1000,"event":{"user":{"text":"Parent task"}}}`), 0o600))
	engine.SyncAllForceParse(t.Context(), nil)
	parent, err := database.GetSessionFull(t.Context(), "fx:z.parent")
	require.NoError(t, err)
	require.NotNil(t, parent)
	child, err = database.GetSessionFull(t.Context(), "fx:a.child")
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, parent.ID, *child.ParentSessionID)
	assert.Equal(t, string(parser.RelSubagent), child.RelationshipType)
	require.NotNil(t, child.FirstMessage)
	assert.Equal(t, "Child task", *child.FirstMessage)
}

func TestFxSyncEqualLengthCompanionRewrites(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "session-a")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	events := filepath.Join(dir, "events.jsonl")
	metadata := filepath.Join(dir, "session.json")
	usageFile := filepath.Join(dir, "usage-v2.json")
	require.NoError(t, os.WriteFile(events, []byte(`{"timestamp_ms":2000,"event":{"assistant":{"text":"Reply","provider_replay":{"source":{"model":"historical","provider":"codex"}}}}}`), 0o600))
	require.NoError(t, os.WriteFile(metadata, []byte(`{"schema_version":4,"title":"Before","model":"old"}`), 0o600))
	require.NoError(t, os.WriteFile(usageFile, []byte(`{"snapshot":{"models":[{"model":"test","output_tokens":5}]}}`), 0o600))
	newer, older := time.Unix(2000, 0), time.Unix(1000, 0)
	require.NoError(t, os.Chtimes(events, newer, newer))
	for _, file := range []string{metadata, usageFile} {
		require.NoError(t, os.Chtimes(file, older, older))
	}
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentFx: {root}}, Machine: "local", DisableSignalRecomputation: true})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	for _, test := range []struct {
		name, file, content, title string
		output                     int
	}{
		{"metadata", metadata, `{"schema_version":4,"title":"After!","model":"new"}`, "After!", 5},
		{"usage", usageFile, `{"snapshot":{"models":[{"model":"test","output_tokens":9}]}}`, "After!", 9},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(test.file, []byte(test.content), 0o600))
			require.NoError(t, os.Chtimes(test.file, older, older))
			engine.SyncAll(t.Context(), nil)
			session, err := database.GetSessionFull(t.Context(), "fx:session-a")
			require.NoError(t, err)
			require.NotNil(t, session)
			require.NotNil(t, session.SessionName)
			assert.Equal(t, test.title, *session.SessionName)
			usage, err := database.GetUsageEvents(t.Context(), "fx:session-a")
			require.NoError(t, err)
			require.Len(t, usage, 1)
			assert.Equal(t, test.output, usage[0].OutputTokens)
		})
	}
	stats := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, stats.Synced)
	assert.GreaterOrEqual(t, stats.Skipped, 1)
}
