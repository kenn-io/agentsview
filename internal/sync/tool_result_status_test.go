package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestClaudeLateToolResultPreservesFailureMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project-a", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	initial := testjsonl.JoinJSONL(
		`{"type":"user","uuid":"u1","timestamp":"2024-01-01T10:00:00Z","message":{"content":"Read the file"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T10:00:01Z","message":{"id":"msg_one","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"missing.go"}}]}}`,
	)
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}, Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	appended := `{"type":"user","uuid":"r1","parentUuid":"a1","timestamp":"2024-01-01T10:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"call-1","content":"File does not exist.","is_error":true}]}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	session, err := database.GetSessionFull(t.Context(), "session")
	require.NoError(t, err)
	require.True(t, session.LastWriteIncremental)
	stored, err := database.GetMessages(t.Context(), "session", 0, 100, true)
	require.NoError(t, err)
	require.Len(t, stored, 2)
	require.Len(t, stored[1].ToolCalls, 1)
	call := stored[1].ToolCalls[0]
	assert.Equal(t, "File does not exist.", call.ResultContent)
	require.Len(t, call.ResultEvents, 1)
	assert.Equal(t, "errored", call.ResultEvents[0].Status)
	assert.Equal(t, "File does not exist.", call.ResultEvents[0].Content)
	assert.Equal(t, "2024-01-01T10:00:02Z", call.ResultEvents[0].Timestamp)
	assert.Equal(t, 1, signals.ComputeToolHealth(ingest.ExtractToolCallRows(stored)).FailureSignalCount)

	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	uploader, ok := provider.(parser.ClaudeUploadParser)
	require.True(t, ok)
	parsed, err := uploader.ParseUploadedTranscript(path, "project-a", "local")
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	candidate, err := ingest.PrepareCandidate(t.Context(), parsed[0], ingest.ContentOptions{})
	require.NoError(t, err)
	assert.Equal(t, candidate.Messages[1].ToolCalls[0].ResultEvents, call.ResultEvents,
		"full parsing and late-result storage must retain the same metadata")

	require.NoError(t, database.SetSessionDataVersionsContext(t.Context(), []string{"session"}, db.CurrentDataVersion()-1))
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced,
		"a stale data version must reparse even when the source is unchanged")
	session, err = database.GetSessionFull(t.Context(), "session")
	require.NoError(t, err)
	assert.False(t, session.LastWriteIncremental)
	stored, err = database.GetAllMessages(t.Context(), "session")
	require.NoError(t, err)
	require.Len(t, stored, 2)
	require.Len(t, stored[1].ToolCalls, 1)
	assert.Equal(t, call.ResultEvents, stored[1].ToolCalls[0].ResultEvents)
}
